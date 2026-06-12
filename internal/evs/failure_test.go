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
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			// Cancel when reconciliation sends its follow-up request.
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

func TestTransientStatusNotRetried(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "bad gateway", status: http.StatusBadGateway},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

// Deletion polling continues after a transient 503.
func TestDeletionToleratesATransientPollFailure(t *testing.T) {
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

func TestClassifiedErrorSentinelAndRedactedText(t *testing.T) {
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
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"network timeout is transient", stubNetError{timeout: true}, evs.ErrUnavailable},
		{
			"refused or unresolvable network failure is terminal",
			stubNetError{timeout: false},
			evs.ErrOperationFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
