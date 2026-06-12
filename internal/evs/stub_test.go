package evs_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

// Tests use a scripted HTTP transport without simulating EVS state.

const (
	stubVolumeID   = "11111111-1111-1111-1111-111111111111"
	stubVolumeName = "pvc-11111111-1111-1111-1111-111111111111"
	stubServerID   = "22222222-2222-2222-2222-222222222222"
	stubDeviceName = "/dev/vdb"
	stubJobID      = "33333333-3333-3333-3333-333333333333"
)

// volumeDetailBody is one volume detail response carrying this driver's ownership marker.
func volumeDetailBody(status string) string {
	return fmt.Sprintf(
		`{"volume":{"id":%q,"name":%q,"status":%q,"size":10,`+
			`"availability_zone":"eu-de-01","volume_type":"SSD","tags":{%q:%q}}}`,
		stubVolumeID,
		stubVolumeName,
		status,
		evs.OwnershipTagKey,
		evs.OwnershipTagValue,
	)
}

// volumeListPageBody is one listing page holding a single volume, so a paged listing keeps advancing.
func volumeListPageBody() string {
	return fmt.Sprintf(
		`{"volumes":[{"id":%q,"name":%q,"status":"available","size":10,`+
			`"availability_zone":"eu-de-01","volume_type":"SSD"}]}`,
		stubVolumeID,
		stubVolumeName,
	)
}

// attachmentsBody is one attachment listing that reports the stub volume attached to the stub server.
func attachmentsBody(device string) string {
	return fmt.Sprintf(
		`{"volumeAttachments":[{"volumeId":%q,"serverId":%q,"device":%q}]}`,
		stubVolumeID,
		stubServerID,
		device,
	)
}

const (
	noAttachmentsBody = `{"volumeAttachments":[]}`
	acceptedJobBody   = `{"job_id":"` + stubJobID + `"}`
	runningJobBody    = `{"status":"RUNNING"}`
	blankJobBody      = `{}`
)

type scriptedAnswer struct {
	status int
	body   string
	err    error
	before func()
}

type observedRequest struct {
	method      string
	url         string
	deadline    time.Time
	hasDeadline bool
}

// stubTransport records requests and returns scripted answers in order, repeating the last.
// It is not safe for concurrent use: each test must own its transport, and requests may only
// be read after the operation under test has returned (mustFinish establishes that ordering).
type stubTransport struct {
	script   []scriptedAnswer
	repeat   scriptedAnswer
	observed []observedRequest
}

// newStubTransport answers each request from script in order and repeats the last entry afterwards.
func newStubTransport(script ...scriptedAnswer) *stubTransport {
	if len(script) == 0 {
		panic("stub transport needs at least one answer")
	}
	return &stubTransport{script: script, repeat: script[len(script)-1]}
}

// RoundTrip records the request and returns the next scripted answer.
func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, hasDeadline := req.Context().Deadline()

	s.observed = append(s.observed, observedRequest{
		method:      req.Method,
		url:         req.URL.String(),
		deadline:    deadline,
		hasDeadline: hasDeadline,
	})
	answer := s.repeat
	if len(s.script) > 0 {
		answer = s.script[0]
		s.script = s.script[1:]
	}

	if answer.before != nil {
		answer.before()
	}
	if answer.err != nil {
		return nil, answer.err
	}

	status := answer.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		Status:     http.StatusText(status),
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(answer.body)),
		Request:    req,
	}, nil
}

// requests returns a copy of the observed requests.
func (s *stubTransport) requests() []observedRequest {
	return append([]observedRequest(nil), s.observed...)
}

func assertRequests(t *testing.T, transport *stubTransport, want ...string) {
	t.Helper()

	requests := transport.requests()
	if len(requests) < len(want) {
		t.Fatalf("expected at least %d requests, got %d", len(want), len(requests))
	}
	for i, fragment := range want {
		method, path, ok := strings.Cut(fragment, " ")
		if !ok || requests[i].method != method || !strings.Contains(requests[i].url, path) {
			t.Fatalf(
				"request %d = %s %s, want %s containing %q",
				i,
				requests[i].method,
				requests[i].url,
				method,
				path,
			)
		}
	}
}

// stubConfig is a credential-shaped configuration that never reaches a network.
func stubConfig() evs.Config {
	return evs.Config{
		AuthURL:    "https://iam.example.invalid/v3",
		AccessKey:  "stub-access-key",
		SecretKey:  "stub-secret-key",
		ProjectID:  "00000000000000000000000000000000",
		RegionName: "eu-de",
	}
}

// newStubClient builds a client using transport with SDK backoff disabled.
func newStubClient(t *testing.T, cfg evs.Config, transport http.RoundTripper) *evs.Client {
	t.Helper()

	provider := new(golangsdk.ProviderClient)
	provider.UseTokenLock()
	noBackoffRetries := 0
	backoffTimeout := time.Duration(0)
	provider.MaxBackoffRetries = &noBackoffRetries
	provider.BackoffRetryTimeout = &backoffTimeout
	provider.HTTPClient = http.Client{Transport: transport}

	serviceClient := func(endpoint string) *golangsdk.ServiceClient {
		return &golangsdk.ServiceClient{ProviderClient: provider, Endpoint: endpoint}
	}

	return evs.NewClientWithServiceClients(
		serviceClient("https://evs.example.invalid/v3/project/"),
		serviceClient("https://evs.example.invalid/v2/project/"),
		serviceClient("https://ecs.example.invalid/v1/project/"),
		cfg,
	)
}

// newSocketClient uses a network connection to test request cancellation.
func newSocketClient(t *testing.T, cfg evs.Config, baseURL string) *evs.Client {
	t.Helper()

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	provider := new(golangsdk.ProviderClient)
	provider.UseTokenLock()
	noBackoffRetries := 0
	backoffTimeout := time.Duration(0)
	provider.MaxBackoffRetries = &noBackoffRetries
	provider.BackoffRetryTimeout = &backoffTimeout
	provider.HTTPClient = http.Client{Transport: transport}

	endpoint := strings.TrimSuffix(baseURL, "/") + "/"
	serviceClient := func() *golangsdk.ServiceClient {
		return &golangsdk.ServiceClient{ProviderClient: provider, Endpoint: endpoint}
	}

	return evs.NewClientWithServiceClients(
		serviceClient(),
		serviceClient(),
		serviceClient(),
		cfg,
	)
}

// mustFinish fails if operation exceeds limit.
func mustFinish(t *testing.T, limit time.Duration, operation func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- operation()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("operation did not return within %s", limit)
		return nil
	}
}
