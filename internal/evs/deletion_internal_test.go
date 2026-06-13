package evs

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
)

// presentVolumeBody is a tagged volume detail that always reports the volume present, so the
// absence poll never sees it go away.
const presentVolumeBody = `{"volume":{"id":"` + absenceVolumeID + `","name":"pvc-absent",` +
	`"status":"available","size":10,"availability_zone":"eu-de-01","volume_type":"SSD",` +
	`"tags":{"` + OwnershipTagKey + `":"` + OwnershipTagValue + `"}}}`

const absenceVolumeID = "44444444-4444-4444-4444-444444444444"

// stubResponse builds a JSON response for req.
func stubResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// expectDeleteAndPolls checks the transport saw one DELETE and one GET per read: the marker and
// status check plus one read per absence attempt.
func expectDeleteAndPolls(t *testing.T, deletes, polls, attempts int) {
	t.Helper()
	if deletes != 1 {
		t.Errorf("expected exactly one delete call, got %d", deletes)
	}
	if want := 1 + attempts; polls != want {
		t.Errorf("expected %d reads, got %d", want, polls)
	}
}

// alwaysPresentTransport accepts the DELETE and then answers every later GET with the volume
// still present, a state the real service never reaches.
type alwaysPresentTransport struct {
	deletes int
	polls   int
}

func (t *alwaysPresentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete {
		t.deletes++
		return stubResponse(req, http.StatusAccepted, `{}`), nil
	}
	t.polls++
	return stubResponse(req, http.StatusOK, presentVolumeBody), nil
}

// absenceClient builds a client over transport with SDK backoff disabled.
func absenceClient(t *testing.T, transport http.RoundTripper) *Client {
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

	return NewClientWithServiceClients(
		serviceClient("https://evs.example.invalid/v3/project/"),
		serviceClient("https://evs.example.invalid/v2/project/"),
		serviceClient("https://ecs.example.invalid/v1/project/"),
		Config{
			AuthURL:    "https://iam.example.invalid/v3",
			AccessKey:  "stub-access-key",
			SecretKey:  "stub-secret-key",
			ProjectID:  "00000000000000000000000000000000",
			RegionName: "eu-de",
		},
	)
}

// shortenAbsencePolling shortens the package poll schedule so the wait exhausts in milliseconds,
// restoring the originals via t.Cleanup.
func shortenAbsencePolling(t *testing.T, attempts int) {
	t.Helper()

	interval, maxAttempts := volumeAbsencePollInterval, volumeAbsenceMaxAttempts
	t.Cleanup(func() {
		volumeAbsencePollInterval, volumeAbsenceMaxAttempts = interval, maxAttempts
	})
	volumeAbsencePollInterval = time.Millisecond
	volumeAbsenceMaxAttempts = attempts
}

// brownoutTransport answers the ownership GET with the volume present, accepts the DELETE and
// returns 503 for every absence poll after it.
type brownoutTransport struct {
	deletes int
	polls   int
}

func (t *brownoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete {
		t.deletes++
		return stubResponse(req, http.StatusAccepted, `{}`), nil
	}
	t.polls++
	if t.deletes == 0 {
		return stubResponse(req, http.StatusOK, presentVolumeBody), nil
	}
	return stubResponse(req, http.StatusServiceUnavailable, `{}`), nil
}

// Exhaustion after only transient poll failures returns ErrUnavailable, not a presence error.
// The test writes package variables, so it cannot run in parallel.
func TestDeletionExhaustedByBrownoutStaysRetryable(t *testing.T) {
	const attempts = 3
	shortenAbsencePolling(t, attempts)

	transport := &brownoutTransport{}
	client := absenceClient(t, transport)

	err := client.DeleteVolume(t.Context(), absenceVolumeID)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected the last transient kind once no poll observed the volume, got: %v", err)
	}
	if strings.Contains(err.Error(), "still present") {
		t.Errorf("expected no claim of observed presence, got: %v", err)
	}
	expectDeleteAndPolls(t, transport.deletes, transport.polls, attempts)
}

// If the absence poll never sees the volume go away, exhaustion is terminal. The real service
// eventually deletes the volume, so only a stub reaches this path. The test writes package
// variables, so it cannot run in parallel.
func TestDeletionWhoseAbsenceNeverArrivesIsTerminal(t *testing.T) {
	const attempts = 3
	shortenAbsencePolling(t, attempts)

	transport := &alwaysPresentTransport{}
	client := absenceClient(t, transport)

	err := client.DeleteVolume(t.Context(), absenceVolumeID)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("expected a terminal failure once absence never arrives, got: %v", err)
	}
	if !strings.Contains(err.Error(), "still present after polling") {
		t.Errorf("expected the error to name the exhausted wait, got: %v", err)
	}
	expectDeleteAndPolls(t, transport.deletes, transport.polls, attempts)
}
