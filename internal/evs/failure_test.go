package evs_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

func TestCancelDuringReconcileReportsCanceled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// observation is returned by every attachment poll.
		observation string
		effect      scriptedAnswer
		invoke      func(context.Context, *evs.Client) error
		// masked is the effect error that must not be returned after cancellation.
		masked error
	}{
		{
			name:        "attach reconciles after a failed effect call",
			observation: noAttachmentsBody,
			effect:      scriptedAnswer{status: http.StatusServiceUnavailable, body: `{}`},
			invoke: func(ctx context.Context, client *evs.Client) error {
				_, err := client.AttachVolume(ctx, stubVolumeID, stubServerID)
				return err
			},
			masked: evs.ErrUnavailable,
		},
		{
			name:        "attach receives a blank job identifier",
			observation: noAttachmentsBody,
			effect:      scriptedAnswer{body: blankJobBody},
			invoke: func(ctx context.Context, client *evs.Client) error {
				_, err := client.AttachVolume(ctx, stubVolumeID, stubServerID)
				return err
			},
			masked: evs.ErrOperationFailed,
		},
		{
			name:        "detach reconciles after a failed effect call",
			observation: attachmentsBody(stubDeviceName),
			effect:      scriptedAnswer{status: http.StatusServiceUnavailable, body: `{}`},
			invoke: func(ctx context.Context, client *evs.Client) error {
				return client.DetachVolume(ctx, stubVolumeID, stubServerID)
			},
			masked: evs.ErrUnavailable,
		},
		{
			name:        "detach receives a blank job identifier",
			observation: attachmentsBody(stubDeviceName),
			effect:      scriptedAnswer{body: blankJobBody},
			invoke: func(ctx context.Context, client *evs.Client) error {
				return client.DetachVolume(ctx, stubVolumeID, stubServerID)
			},
			masked: evs.ErrOperationFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			// Cancel on the follow-up request so the reported cause is the
			// caller's stop, not the effect that started reconciliation.
			transport := newStubTransport(
				scriptedAnswer{body: tc.observation},
				tc.effect,
				scriptedAnswer{body: tc.observation, before: cancel},
			)
			client := newStubClient(t, stubConfig(), transport)

			err := mustFinish(t, 10*time.Second, func() error {
				return tc.invoke(ctx, client)
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected the reported cause to be cancellation, got: %v", err)
			}
			if errors.Is(err, tc.masked) {
				t.Fatalf("cancellation was reported as %v instead: %v", tc.masked, err)
			}
		})
	}
}

// refusedAttachBody is the compute 400 returned when an attach was already accepted once. The
// attachment listing can still be empty then, so the volume record must be checked before trusting
// the refusal.
const refusedAttachBody = `{"error":{"message":"the volume has already been attached to this instance` +
	` and you cannot repeatedly attach.","code":"Ecs.0005","details":[{"code":"Ecs.0057"}]}}`

// A canceled attach can leave compute holding an attachment the listing does not show yet.
// Re-issuing is refused; the caller must get the established attachment, not the refusal.
func TestRefusedAttachReconcilesAnAcceptedAttachment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// detail is the volume record answer; it decides whether the refusal stands.
		detail string
	}{
		{
			name:   "the volume lists the attachment",
			detail: volumeDetailBodyWithAttachment("attaching", stubServerID, ""),
		},
		{
			name:   "the volume reports only that an attach is under way",
			detail: volumeDetailBody("attaching"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newStubTransport(
				scriptedAnswer{body: noAttachmentsBody},
				scriptedAnswer{status: http.StatusBadRequest, body: refusedAttachBody},
				scriptedAnswer{body: tc.detail},
				scriptedAnswer{body: attachmentsBody(stubDeviceName)},
			)
			client := newStubClient(t, stubConfig(), transport)

			var attachment *evs.Attachment
			err := mustFinish(t, 10*time.Second, func() error {
				observed, err := client.AttachVolume(t.Context(), stubVolumeID, stubServerID)
				attachment = observed
				return err
			})
			if err != nil {
				t.Fatalf(
					"a refused attach backed by the volume record must reconcile, got: %v",
					err,
				)
			}
			if attachment == nil || attachment.DeviceName != stubDeviceName {
				t.Fatalf("expected the established attachment, got: %+v", attachment)
			}

			assertRequests(
				t,
				transport,
				"GET /block_device",
				"POST /attachvolume",
				"GET /os-vendor-volumes/"+stubVolumeID,
				"GET /block_device",
			)
		})
	}
}

// The same refusal against a volume holding no attach is a rejected request; against a volume
// another server holds it is a conflict. Both must fail fast.
func TestRefusedAttachTheVolumeDeniesStaysRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		detail string
		want   error
	}{
		{
			name:   "the volume is available and unattached",
			detail: volumeDetailBody("available"),
			want:   evs.ErrInvalidArgument,
		},
		{
			name: "the volume is attached to another server",
			detail: volumeDetailBodyWithAttachment(
				"in-use",
				"44444444-4444-4444-4444-444444444444",
				"/dev/vdc",
			),
			want: evs.ErrConflict,
		},
		{
			name: "the volume is attaching to another server",
			detail: volumeDetailBodyWithAttachment(
				"attaching",
				"44444444-4444-4444-4444-444444444444",
				"",
			),
			want: evs.ErrConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newStubTransport(
				scriptedAnswer{body: noAttachmentsBody},
				scriptedAnswer{status: http.StatusBadRequest, body: refusedAttachBody},
				scriptedAnswer{body: tc.detail},
			)
			client := newStubClient(t, stubConfig(), transport)

			err := mustFinish(t, 10*time.Second, func() error {
				_, err := client.AttachVolume(t.Context(), stubVolumeID, stubServerID)
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("AttachVolume() = %v, want %v", err, tc.want)
			}

			assertRequests(
				t,
				transport,
				"GET /block_device",
				"POST /attachvolume",
				"GET /os-vendor-volumes/"+stubVolumeID,
			)
			if requests := len(transport.requests()); requests != 3 {
				t.Fatalf(
					"a refusal the volume record does not back must fail fast, saw %d requests",
					requests,
				)
			}
		})
	}
}

func TestRefusedAttachProbePreservesCallerCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	transport := newStubTransport(
		scriptedAnswer{body: noAttachmentsBody},
		scriptedAnswer{status: http.StatusBadRequest, body: refusedAttachBody},
		scriptedAnswer{before: cancel, err: context.Canceled},
	)
	client := newStubClient(t, stubConfig(), transport)

	_, err := client.AttachVolume(ctx, stubVolumeID, stubServerID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AttachVolume() = %v, want caller cancellation", err)
	}
	if errors.Is(err, evs.ErrInvalidArgument) {
		t.Fatalf("AttachVolume() preserved the refusal over cancellation: %v", err)
	}
}

func TestTerminalAttachRefusalsDoNotProbeVolumeDetail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   error
	}{
		{status: http.StatusUnauthorized, want: evs.ErrUnauthenticated},
		{status: http.StatusForbidden, want: evs.ErrPermissionDenied},
		{status: http.StatusNotFound, want: evs.ErrNotFound},
		{status: http.StatusConflict, want: evs.ErrConflict},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()

			transport := newStubTransport(
				scriptedAnswer{body: noAttachmentsBody},
				scriptedAnswer{status: tc.status, body: `{}`},
			)
			client := newStubClient(t, stubConfig(), transport)

			_, err := client.AttachVolume(t.Context(), stubVolumeID, stubServerID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("AttachVolume() = %v, want %v", err, tc.want)
			}
			if got := len(transport.requests()); got != 2 {
				t.Fatalf(
					"AttachVolume() made %d requests, want no detail probe after the refusal",
					got,
				)
			}
		})
	}
}

func TestTransientStatusNotRetried(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
	}{
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "bad gateway", status: http.StatusBadGateway},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newStubTransport(scriptedAnswer{status: tc.status, body: `{}`})
			client := newStubClient(t, stubConfig(), transport)

			err := mustFinish(t, 10*time.Second, func() error {
				_, err := client.CreateVolume(t.Context(), evs.CreateVolumeOpts{
					AvailabilityZone: "eu-de-01",
					VolumeType:       "SSD",
					Size:             10,
				})
				return err
			})
			if !errors.Is(err, evs.ErrUnavailable) {
				t.Fatalf("expected a transient failure, got: %v", err)
			}
			if requests := len(transport.requests()); requests != 1 {
				t.Fatalf(
					"a non-idempotent request must not be repeated, saw %d requests",
					requests,
				)
			}
			if request := transport.requests()[0]; request.method != http.MethodPost {
				t.Fatalf(
					"expected the non-idempotent create request, got %s %s",
					request.method,
					request.url,
				)
			}
		})
	}
}

func TestCreateVolumeReadsVolumeIDFromTheJobItAlreadyPolled(t *testing.T) {
	t.Parallel()

	const v3Host = "evs-v3.example.invalid"
	transport := newStubTransport(
		scriptedAnswer{body: acceptedJobBody},
		scriptedAnswer{body: successJobBody},
		scriptedAnswer{body: volumeDetailBody("available")},
	)
	client := newStubClientWithEndpoints(
		t,
		stubConfig(),
		transport,
		"https://"+v3Host+"/v3/project/",
		"https://evs.example.invalid/v2/project/",
		"https://ecs.example.invalid/v1/project/",
	)

	var created *evs.Volume
	err := mustFinish(t, 10*time.Second, func() error {
		vol, createErr := client.CreateVolume(t.Context(), evs.CreateVolumeOpts{
			AvailabilityZone: "eu-de-01",
			VolumeType:       "SSD",
			Size:             10,
		})
		created = vol
		return createErr
	})
	if err != nil {
		t.Fatalf("CreateVolume() = %v", err)
	}
	if created == nil || created.ID != stubVolumeID {
		t.Fatalf("CreateVolume() volume = %+v, want id %s", created, stubVolumeID)
	}

	requests := transport.requests()
	if len(requests) != 3 {
		t.Fatalf(
			"CreateVolume() made %d requests, want create, one job poll, and get volume",
			len(requests),
		)
	}
	assertRequests(
		t,
		transport,
		"POST /cloudvolumes",
		"GET /jobs/",
		"GET /os-vendor-volumes/"+stubVolumeID,
	)

	jobURL := requests[1].url
	if !strings.Contains(jobURL, v3Host) {
		t.Fatalf("job poll rewrote the host, got %s", jobURL)
	}
	if !strings.Contains(jobURL, "/v1/") {
		t.Fatalf("job poll did not use the v1 job path, got %s", jobURL)
	}
	for i, request := range requests {
		if strings.Contains(request.url, "evs-v1.example.invalid") {
			t.Fatalf("request %d used the SDK host rewrite: %s %s", i, request.method, request.url)
		}
	}
}

func TestCreateVolumeToleratesATransientJobPoll(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer scriptedAnswer
	}{
		{
			name:   "service unavailable",
			answer: scriptedAnswer{status: http.StatusServiceUnavailable, body: `{}`},
		},
		{name: "network timeout", answer: scriptedAnswer{err: stubNetError{timeout: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newStubTransport(
				scriptedAnswer{body: acceptedJobBody},
				tc.answer,
				scriptedAnswer{body: successJobBody},
				scriptedAnswer{body: volumeDetailBody("available")},
			)
			client := newStubClient(t, stubConfig(), transport)

			var created *evs.Volume
			err := mustFinish(t, 10*time.Second, func() error {
				vol, createErr := client.CreateVolume(t.Context(), evs.CreateVolumeOpts{
					AvailabilityZone: "eu-de-01",
					VolumeType:       "SSD",
					Size:             10,
				})
				created = vol
				return createErr
			})
			if err != nil {
				t.Fatalf("CreateVolume() = %v", err)
			}
			if created == nil || created.ID != stubVolumeID {
				t.Fatalf("CreateVolume() volume = %+v, want id %s", created, stubVolumeID)
			}
			assertRequests(
				t,
				transport,
				"POST /cloudvolumes",
				"GET /jobs/",
				"GET /jobs/",
				"GET /os-vendor-volumes/"+stubVolumeID,
			)
		})
	}
}

func TestAttachVolumeToleratesATransientJobPoll(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer scriptedAnswer
	}{
		{
			name:   "service unavailable",
			answer: scriptedAnswer{status: http.StatusServiceUnavailable, body: `{}`},
		},
		{name: "network timeout", answer: scriptedAnswer{err: stubNetError{timeout: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newStubTransport(
				scriptedAnswer{body: noAttachmentsBody},
				scriptedAnswer{body: acceptedJobBody},
				tc.answer,
				scriptedAnswer{body: `{"status":"SUCCESS"}`},
				scriptedAnswer{body: attachmentsBody(stubDeviceName)},
			)
			client := newStubClient(t, stubConfig(), transport)

			var attachment *evs.Attachment
			err := mustFinish(t, 10*time.Second, func() error {
				observed, attachErr := client.AttachVolume(t.Context(), stubVolumeID, stubServerID)
				attachment = observed
				return attachErr
			})
			if err != nil {
				t.Fatalf("AttachVolume() = %v", err)
			}
			if attachment == nil || attachment.DeviceName != stubDeviceName {
				t.Fatalf("AttachVolume() = %+v, want device %s", attachment, stubDeviceName)
			}
			assertRequests(
				t,
				transport,
				"GET /block_device",
				"POST /attachvolume",
				"GET /jobs/",
				"GET /jobs/",
				"GET /block_device",
			)
		})
	}
}

// A 503 during the absence wait is transient. The next poll can still observe
// that the volume is gone.
func TestDeletionToleratesATransientPollFailure(t *testing.T) {
	t.Parallel()

	transport := newStubTransport(
		scriptedAnswer{body: volumeDetailBody("available")},
		scriptedAnswer{status: http.StatusAccepted},
		scriptedAnswer{status: http.StatusServiceUnavailable, body: `{}`},
		scriptedAnswer{status: http.StatusNotFound, body: `{}`},
	)
	client := newStubClient(t, stubConfig(), transport)

	err := mustFinish(t, 10*time.Second, func() error {
		return client.DeleteVolume(t.Context(), stubVolumeID)
	})
	if err != nil {
		t.Fatalf("a transient poll failure must not fail the deletion, got: %v", err)
	}
}

// error_deleting is already terminal. A DELETE against that state would ask the cloud to remove
// a volume whose deletion already failed.
func TestDeletionReportsAnAlreadyFailedDeletion(t *testing.T) {
	t.Parallel()

	transport := newStubTransport(scriptedAnswer{body: volumeDetailBody("error_deleting")})
	client := newStubClient(t, stubConfig(), transport)

	err := mustFinish(t, 10*time.Second, func() error {
		return client.DeleteVolume(t.Context(), stubVolumeID)
	})
	if !errors.Is(err, evs.ErrOperationFailed) {
		t.Fatalf("expected a terminal failure for an already-failed deletion, got: %v", err)
	}

	for _, request := range transport.requests() {
		if request.method == http.MethodDelete {
			t.Fatalf(
				"a failed deletion must not be re-issued, saw %s %s",
				request.method,
				request.url,
			)
		}
	}
}

func TestClassifiedErrorSentinelAndRedactedText(t *testing.T) {
	t.Parallel()

	// One secret contains the other, which is what makes replacement order observable.
	cfg := stubConfig()
	cfg.AccessKey = "QQQQ1234"
	cfg.SecretKey = "QQQQ"

	inspected := fmt.Errorf(
		"cloud detail %s and %s on DISK: /dev/sda1: %w",
		cfg.AccessKey,
		cfg.SecretKey,
		evs.ErrConflict,
	)
	transport := newStubTransport(scriptedAnswer{err: inspected})
	client := newStubClient(t, cfg, transport)

	_, err := client.GetVolume(t.Context(), stubVolumeID)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, evs.ErrConflict) {
		t.Fatalf("expected the matched category to be preserved, got: %v", err)
	}
	if errors.Is(err, inspected) {
		t.Fatalf("the inspected error chain reached the caller: %v", err)
	}

	message := err.Error()
	if strings.Contains(message, cfg.AccessKey) || strings.Contains(message, cfg.SecretKey) {
		t.Fatalf("returned message contains a secret: %s", message)
	}
	if strings.Contains(message, "1234") {
		t.Fatalf("returned message contains part of an overlapping secret: %s", message)
	}
	if !strings.Contains(message, "/dev/sda1") {
		t.Fatalf("expected the device path to survive redaction, got: %s", message)
	}
}

func TestCanceledErrorRedactsSecrets(t *testing.T) {
	t.Parallel()

	cfg := stubConfig()
	cfg.SecretKey = "QQQQ"

	inspected := fmt.Errorf("cloud detail %s: %w", cfg.SecretKey, context.Canceled)
	transport := newStubTransport(scriptedAnswer{err: inspected})
	client := newStubClient(t, cfg, transport)

	_, err := client.GetVolume(t.Context(), stubVolumeID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the reported cause to be cancellation, got: %v", err)
	}
	if strings.Contains(err.Error(), cfg.SecretKey) {
		t.Fatalf("cancellation report contains a secret: %s", err.Error())
	}
}

// stubNetError is a network failure whose Timeout is scripted.
type stubNetError struct {
	timeout bool
}

func (e stubNetError) Error() string   { return "stub network error" }
func (e stubNetError) Timeout() bool   { return e.timeout }
func (e stubNetError) Temporary() bool { return e.timeout }

func TestNetworkTimeoutOnlyIsTransient(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "network timeout is transient",
			err:  stubNetError{timeout: true},
			want: evs.ErrUnavailable,
		},
		{
			name: "refused or unresolvable network failure is terminal",
			err:  stubNetError{timeout: false},
			want: evs.ErrOperationFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newStubTransport(scriptedAnswer{err: tc.err})
			client := newStubClient(t, stubConfig(), transport)

			_, err := client.GetVolume(t.Context(), stubVolumeID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got: %v", tc.want, err)
			}
			if tc.want != evs.ErrUnavailable && errors.Is(err, evs.ErrUnavailable) {
				t.Fatalf("a terminal network failure was reported as transient: %v", err)
			}
		})
	}
}
