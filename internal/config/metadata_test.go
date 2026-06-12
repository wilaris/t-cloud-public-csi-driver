package config

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testServerUUID       = "9f0a1b2c-3d4e-5f60-7182-93a4b5c6d7e8"
	testAvailabilityZone = "eu-de-01"
)

// metadataAnswer is one scripted reply from the stub metadata service.
type metadataAnswer struct {
	status int
	body   string
}

// newScriptedMetadataServer answers each request with the next scripted answer, repeating the last
// one for every further request, so a test states only the prefix it cares about. The returned
// client is the production metadata client pointed at the server. The counter is atomic because the
// handler runs on the server's goroutine while the test reads it on its own.
func newScriptedMetadataServer(
	t *testing.T,
	answers ...metadataAnswer,
) (*metadataClient, *atomic.Int64) {
	t.Helper()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen := int(requests.Add(1))
		answer := answers[min(seen, len(answers))-1]
		w.WriteHeader(answer.status)
		_, _ = w.Write([]byte(answer.body))
	}))
	t.Cleanup(server.Close)

	client := newMetadataClient()
	client.baseURL = server.URL
	return client, &requests
}

// newMetadataServer answers every request the same way.
func newMetadataServer(t *testing.T, status int, body string) (*metadataClient, *atomic.Int64) {
	t.Helper()
	return newScriptedMetadataServer(t, metadataAnswer{status: status, body: body})
}

func metadataBody(uuid, zone string) string {
	return fmt.Sprintf(`{"uuid":%q,"availability_zone":%q}`, uuid, zone)
}

func TestMetadataFetchTrimsDocumentValues(t *testing.T) {
	t.Parallel()

	client, _ := newMetadataServer(
		t,
		http.StatusOK,
		metadataBody(" "+testServerUUID+" ", "\t"+testAvailabilityZone+"\n"),
	)

	facts, err := client.fetch(t.Context())
	if err != nil {
		t.Fatalf("expected a clean fetch, got: %v", err)
	}
	if facts.serverUUID != testServerUUID {
		t.Errorf("expected server UUID %q, got %q", testServerUUID, facts.serverUUID)
	}
	if facts.availabilityZone != testAvailabilityZone {
		t.Errorf(
			"expected availability zone %q, got %q",
			testAvailabilityZone,
			facts.availabilityZone,
		)
	}
}

func TestMetadataFetchNonSuccessStatus(t *testing.T) {
	t.Parallel()

	client, _ := newMetadataServer(t, http.StatusNotFound, `{}`)

	if _, err := client.fetch(t.Context()); err == nil {
		t.Fatal("expected a non-success status to fail the fetch")
	}
}

func TestMetadataFetchRedirectNotFollowed(t *testing.T) {
	t.Parallel()

	const declinedLocation = "http://elsewhere.invalid/meta"

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, declinedLocation, http.StatusFound)
	}))
	t.Cleanup(server.Close)

	client := newMetadataClient()
	client.baseURL = server.URL

	_, err := client.fetch(t.Context())
	if err == nil {
		t.Fatal("expected a redirect to fail the fetch")
	}
	if strings.Contains(err.Error(), declinedLocation) {
		t.Errorf("error leaked the declined redirect location: %v", err)
	}
	// Redirects are neither followed nor retried.
	if observed := requests.Load(); observed != 1 {
		t.Errorf("expected the redirect neither followed nor retried, saw %d requests", observed)
	}
}

func TestMetadataFetchOversizedDocument(t *testing.T) {
	t.Parallel()

	oversized := `{"padding":"` + strings.Repeat("x", metadataBodyLimit) + `","uuid":"u"}`
	client, _ := newMetadataServer(t, http.StatusOK, oversized)

	if _, err := client.fetch(t.Context()); err == nil {
		t.Fatal("expected an oversized document to fail the fetch")
	}
}

func TestMetadataFetchMalformedDocument(t *testing.T) {
	t.Parallel()

	client, _ := newMetadataServer(t, http.StatusOK, `{"uuid": not-json`)

	if _, err := client.fetch(t.Context()); err == nil {
		t.Fatal("expected a malformed document to fail the fetch")
	}
}

func TestMetadataFetchRetriesUntilTheServiceAnswers(t *testing.T) {
	t.Parallel()

	client, requests := newScriptedMetadataServer(
		t,
		metadataAnswer{status: http.StatusServiceUnavailable, body: `{}`},
		metadataAnswer{status: http.StatusInternalServerError, body: `{}`},
		metadataAnswer{
			status: http.StatusOK,
			body:   metadataBody(testServerUUID, testAvailabilityZone),
		},
	)

	facts, err := client.fetch(t.Context())
	if err != nil {
		t.Fatalf("expected retrieval to survive two transient answers, got: %v", err)
	}
	if facts.serverUUID != testServerUUID {
		t.Errorf("expected server UUID %q, got %q", testServerUUID, facts.serverUUID)
	}
	if facts.availabilityZone != testAvailabilityZone {
		t.Errorf(
			"expected availability zone %q, got %q",
			testAvailabilityZone,
			facts.availabilityZone,
		)
	}
	if observed := requests.Load(); observed != 3 {
		t.Errorf("expected three attempts, saw %d", observed)
	}
}

func TestMetadataFetchRetriesATransportFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) > 1 {
			_, _ = w.Write([]byte(metadataBody(testServerUUID, testAvailabilityZone)))
			return
		}
		// Drop the connection without answering
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("expected the stub to hijack its connection, got: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)

	client := newMetadataClient()
	client.baseURL = server.URL

	facts, err := client.fetch(t.Context())
	if err != nil {
		t.Fatalf("expected retrieval to survive a transport failure, got: %v", err)
	}
	if facts.serverUUID != testServerUUID {
		t.Errorf("expected server UUID %q, got %q", testServerUUID, facts.serverUUID)
	}
	if observed := requests.Load(); observed != 2 {
		t.Errorf("expected the failed connection to be retried once, saw %d attempts", observed)
	}
}

func TestMetadataFetchDoesNotRetryATerminalAnswer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer metadataAnswer
	}{
		{"a rejected request", metadataAnswer{status: http.StatusNotFound, body: `{}`}},
		{"a refused request", metadataAnswer{status: http.StatusForbidden, body: `{}`}},
		{
			"a malformed document",
			metadataAnswer{status: http.StatusOK, body: `{"uuid": not-json`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, requests := newScriptedMetadataServer(t, tc.answer)

			if _, err := client.fetch(t.Context()); err == nil {
				t.Fatal("expected the answer to fail retrieval")
			}
			if observed := requests.Load(); observed != 1 {
				t.Errorf("expected the answer not to be retried, saw %d attempts", observed)
			}
		})
	}
}

func TestMetadataFetchStopsRetryingWhenTheCallerIsDone(t *testing.T) {
	t.Parallel()

	client, requests := newMetadataServer(t, http.StatusServiceUnavailable, `{}`)

	// Shorter than one backoff wait so cancel hits during the retry delay.
	ctx, cancel := context.WithTimeout(t.Context(), metadataRetryBase/2)
	defer cancel()

	start := time.Now()
	_, err := client.fetch(ctx)
	if err == nil {
		t.Fatal("expected a canceled retrieval to fail")
	}
	if waited := time.Since(start); waited >= metadataTimeout {
		t.Errorf("retrieval ignored the caller and ran for %s", waited)
	}
	if observed := requests.Load(); observed != 1 {
		t.Errorf("expected retrying to stop after the first attempt, saw %d", observed)
	}
	// The wait itself is what the caller interrupts, so the failure keeps the answer that prompted
	// the retry alongside the cause. Abandoning only at the next attempt would report that
	// attempt's transport failure instead, and lose the status.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the caller's cause to be reported, got: %v", err)
	}
	prompting := fmt.Sprintf("status %d", http.StatusServiceUnavailable)
	if !strings.Contains(err.Error(), prompting) {
		t.Errorf("expected the answer that prompted the retry to be kept, got: %v", err)
	}
}

func TestMetadataFetchStopsAtItsOwnDeadline(t *testing.T) {
	t.Parallel()

	client, requests := newMetadataServer(t, http.StatusServiceUnavailable, `{}`)

	start := time.Now()
	if _, err := client.fetch(context.WithoutCancel(t.Context())); err == nil {
		t.Fatal("expected a service that never answers to fail retrieval")
	}

	waited := time.Since(start)
	if waited < metadataTimeout {
		t.Errorf("retrieval gave up after %s, before its own deadline", waited)
	}
	if waited > 2*metadataTimeout {
		t.Errorf("retrieval ran for %s, well past its own deadline", waited)
	}
	if observed := requests.Load(); observed < 2 {
		t.Errorf("expected repeated attempts within the deadline, saw %d", observed)
	}
}

func TestResolveNodeFactsMergesExplicitInputsWithMetadata(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		nodeID       string
		zone         string
		wantNodeID   string
		wantZone     string
		wantRequests int
	}{
		{
			name:         "both explicit inputs skip the metadata request",
			nodeID:       "explicit-uuid",
			zone:         "explicit-zone",
			wantNodeID:   "explicit-uuid",
			wantZone:     "explicit-zone",
			wantRequests: 0,
		},
		{
			name:         "an explicit node ID still discovers the zone",
			nodeID:       "explicit-uuid",
			wantNodeID:   "explicit-uuid",
			wantZone:     testAvailabilityZone,
			wantRequests: 1,
		},
		{
			name:         "an explicit zone still discovers the node ID",
			zone:         "explicit-zone",
			wantNodeID:   testServerUUID,
			wantZone:     "explicit-zone",
			wantRequests: 1,
		},
		{
			name:         "no explicit input takes both facts from metadata",
			wantNodeID:   testServerUUID,
			wantZone:     testAvailabilityZone,
			wantRequests: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, requests := newMetadataServer(
				t,
				http.StatusOK,
				metadataBody(testServerUUID, testAvailabilityZone),
			)
			cfg := &Config{Role: RoleNode, NodeID: tc.nodeID, AvailabilityZone: tc.zone}

			if err := cfg.resolveNodeFacts(t.Context(), client); err != nil {
				t.Fatalf("expected node facts to resolve, got: %v", err)
			}
			if cfg.NodeID != tc.wantNodeID {
				t.Errorf("expected NodeID %q, got %q", tc.wantNodeID, cfg.NodeID)
			}
			if cfg.AvailabilityZone != tc.wantZone {
				t.Errorf("expected AvailabilityZone %q, got %q", tc.wantZone, cfg.AvailabilityZone)
			}
			if requests.Load() != int64(tc.wantRequests) {
				t.Errorf("expected %d metadata requests, saw %d", tc.wantRequests, requests.Load())
			}
		})
	}
}

func TestResolveNodeFactsFetchFailureLeavesNoPartialIdentity(t *testing.T) {
	t.Parallel()

	client, _ := newMetadataServer(t, http.StatusNotFound, `{}`)
	cfg := &Config{Role: RoleNode}

	if err := cfg.resolveNodeFacts(t.Context(), client); err == nil {
		t.Fatal("expected a failed retrieval to fail resolution")
	}
	if cfg.NodeID != "" || cfg.AvailabilityZone != "" {
		t.Errorf(
			"a failed retrieval left a partial identity: %q, %q",
			cfg.NodeID,
			cfg.AvailabilityZone,
		)
	}
}

func TestResolveNodeFactsIncompleteDocument(t *testing.T) {
	t.Parallel()

	client, _ := newMetadataServer(t, http.StatusOK, metadataBody("", testAvailabilityZone))
	cfg := &Config{Role: RoleNode}

	if err := cfg.resolveNodeFacts(t.Context(), client); err == nil {
		t.Fatal("expected a document without a server UUID to fail resolution")
	}
}

func TestResolveNodeFactsControllerNoOp(t *testing.T) {
	t.Parallel()

	client, requests := newMetadataServer(t, http.StatusOK, `{}`)
	cfg := &Config{Role: RoleController}

	if err := cfg.resolveNodeFacts(t.Context(), client); err != nil {
		t.Fatalf("expected the controller role to skip resolution, got: %v", err)
	}
	if requests.Load() != 0 {
		t.Errorf("controller role contacted the metadata service %d times", requests.Load())
	}
}
