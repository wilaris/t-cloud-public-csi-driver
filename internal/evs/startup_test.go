package evs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

// Startup redirect and transient-status behavior.

func TestStartupTransientStatusesAreNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int64

			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.WriteHeader(status)
				}),
			)
			defer server.Close()

			cfg := stubConfig()
			cfg.AuthURL = server.URL

			ctx := t.Context()
			failed := make(chan error, 1)
			go func() {
				_, err := evs.NewProviderClient(ctx, cfg)
				failed <- err
			}()

			select {
			case err := <-failed:
				if err == nil {
					t.Fatal("expected the startup exchange to fail")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the startup exchange retried instead of surfacing the failure")
			}

			if observed := requests.Load(); observed != 1 {
				t.Fatalf("expected exactly one startup request, got %d", observed)
			}
		})
	}
}

func TestStartupAuthenticationStopsWhenTheCallerCancels(t *testing.T) {
	// Server never responds; cancel must abort before requestTimeout.
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer close(released)
	defer server.Close()

	cfg := stubConfig()
	cfg.AuthURL = server.URL

	ctx, cancel := context.WithCancel(t.Context())
	failed := make(chan error, 1)
	go func() {
		_, err := evs.NewProviderClient(ctx, cfg)
		failed <- err
	}()

	cancel()

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("expected the canceled startup exchange to fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the startup exchange ignored the caller's cancellation")
	}
}

func TestStartupContextDoesNotBindLaterRequests(t *testing.T) {
	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/catalog") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"catalog":[]}`))
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	cfg := stubConfig()
	// Versioned identity URL skips discovery; one catalog response is enough for a usable provider.
	cfg.AuthURL = server.URL + "/v3/"

	ctx, cancel := context.WithCancel(t.Context())
	provider, err := evs.NewProviderClient(ctx, cfg)
	if err != nil {
		t.Fatalf("expected startup to succeed against a versioned endpoint: %v", err)
	}

	// Canceling the startup ctx must not affect later provider requests.
	cancel()

	resp, err := provider.Request("GET", server.URL, &golangsdk.RequestOpts{OkCodes: []int{200}})
	if err != nil {
		t.Fatalf("a request issued after startup inherited the startup context: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if observed := requests.Load(); observed != 1 {
		t.Fatalf("expected the request to reach the server once, got %d", observed)
	}
}

func TestStartupRefusesARedirectOffTheConfiguredEndpoint(t *testing.T) {
	var elsewhere atomic.Int64

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/versions", http.StatusFound)
	}))
	defer configured.Close()

	cfg := stubConfig()
	cfg.AuthURL = configured.URL

	_, err := evs.NewProviderClient(t.Context(), cfg)
	if err == nil {
		t.Fatal("expected a redirect off the configured endpoint to fail the exchange")
	}
	if hops := elsewhere.Load(); hops != 0 {
		t.Fatalf("a request reached the redirect target %d times", hops)
	}

	target := strings.TrimPrefix(other.URL, "http://")
	if strings.Contains(err.Error(), target) {
		t.Fatalf("redirect error exposes the target: %v", err)
	}
}
