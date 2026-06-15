// Package evs_test drives the EVS client from outside the package; HTTP is answered from a
// script, not a stand-in cloud.
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

const (
	stubVolumeID   = "11111111-1111-1111-1111-111111111111"
	stubVolumeName = "pvc-11111111-1111-1111-1111-111111111111"
	stubServerID   = "22222222-2222-2222-2222-222222222222"
	stubDeviceName = "/dev/vdb"
	stubJobID      = "33333333-3333-3333-3333-333333333333"
)

// volumeDetailBody is a volume-show body that already carries this driver's
// ownership marker, so later checks do not fail as unowned.
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

// volumeDetailBodyWithAttachment is a volume-show body whose attachments
// array names a server. That is how a volume reports an attach the compute
// listing has not published yet.
func volumeDetailBodyWithAttachment(status, serverID, device string) string {
	return fmt.Sprintf(
		`{"volume":{"id":%q,"name":%q,"status":%q,"size":10,`+
			`"availability_zone":"eu-de-01","volume_type":"SSD","tags":{%q:%q},`+
			`"attachments":[{"volume_id":%q,"server_id":%q,"device":%q}]}}`,
		stubVolumeID,
		stubVolumeName,
		status,
		evs.OwnershipTagKey,
		evs.OwnershipTagValue,
		stubVolumeID,
		serverID,
		device,
	)
}

// volumeListPageBody is one listing page holding a single volume, so a paged
// walk has a reason to request the next offset.
func volumeListPageBody() string {
	return fmt.Sprintf(
		`{"volumes":[{"id":%q,"name":%q,"status":"available","size":10,`+
			`"availability_zone":"eu-de-01","volume_type":"SSD"}]}`,
		stubVolumeID,
		stubVolumeName,
	)
}

// attachmentsBody is a compute attachment listing that reports the stub
// volume on the stub server at the given device path.
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
	successJobBody    = `{"status":"SUCCESS","entities":{"volume_id":"` + stubVolumeID + `"}}`
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

// stubTransport records each request and returns scripted answers in order,
// then the last answer forever. One test owns one transport. requests() is
// only safe after the operation under test has returned; mustFinish is the
// sequencing point.
type stubTransport struct {
	script   []scriptedAnswer
	repeat   scriptedAnswer
	observed []observedRequest
}

// newStubTransport answers from script in order and then repeats the last
// entry. An empty script is a test-authoring error.
func newStubTransport(script ...scriptedAnswer) *stubTransport {
	if len(script) == 0 {
		panic("stub transport needs at least one answer")
	}
	return &stubTransport{script: script, repeat: script[len(script)-1]}
}

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

// requests returns a copy so later assertions cannot mutate the log.
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

// stubConfig is a well-formed credential set that must never leave the process.
func stubConfig() evs.Config {
	return evs.Config{
		AuthURL:    "https://iam.example.invalid/v3",
		AccessKey:  "stub-access-key",
		SecretKey:  "stub-secret-key",
		ProjectID:  "00000000000000000000000000000000",
		RegionName: "eu-de",
	}
}

// newStubClient builds a client over transport with SDK backoff disabled so
// scripted answers are not retried by the SDK itself.
func newStubClient(t *testing.T, cfg evs.Config, transport http.RoundTripper) *evs.Client {
	t.Helper()
	return newStubClientWithEndpoints(
		t,
		cfg,
		transport,
		"https://evs.example.invalid/v3/project/",
		"https://evs.example.invalid/v2/project/",
		"https://ecs.example.invalid/v1/project/",
	)
}

// newStubClientWithEndpoints is newStubClient with caller-chosen service endpoints.
func newStubClientWithEndpoints(
	t *testing.T,
	cfg evs.Config,
	transport http.RoundTripper,
	v3Endpoint, v2Endpoint, ecsEndpoint string,
) *evs.Client {
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
		serviceClient(v3Endpoint),
		serviceClient(v2Endpoint),
		serviceClient(ecsEndpoint),
		cfg,
	)
}

// newSocketClient uses a real transport so a canceled context can abort an in-flight request on
// an open connection.
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

// mustFinish fails the test if operation does not return within limit.
func mustFinish(t *testing.T, limit time.Duration, operation func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- operation()
	}()

	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
		t.Fatalf("operation did not return within %s", limit)
		return nil
	}
}
