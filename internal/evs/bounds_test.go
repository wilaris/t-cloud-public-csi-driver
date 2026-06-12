package evs_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

// requestDeadline returns the deadline observed by the transport.
func requestDeadline(ctx context.Context, t *testing.T) (time.Time, bool) {
	t.Helper()

	transport := newStubTransport(scriptedAnswer{body: volumeDetailBody("available")})
	client := newStubClient(t, stubConfig(), transport)

	if _, err := client.GetVolume(ctx, stubVolumeID); err != nil {
		t.Fatalf("get volume: %v", err)
	}

	requests := transport.requests()
	if len(requests) != 1 {
		t.Fatalf("expected exactly one request, got %d", len(requests))
	}
	return requests[0].deadline, requests[0].hasDeadline
}

func TestOperationDeadlineBounds(t *testing.T) {
	start := time.Now()

	absent, absentOK := requestDeadline(t.Context(), t)

	callerDeadline := start.Add(time.Hour)
	ctx, cancel := context.WithDeadline(t.Context(), callerDeadline)
	defer cancel()
	overlong, overlongOK := requestDeadline(ctx, t)

	if !absentOK || !absent.After(start) || !absent.Before(callerDeadline) {
		t.Errorf("absent caller deadline produced invalid request deadline %s", absent)
	}
	if !overlongOK || !overlong.After(start) || !overlong.Before(callerDeadline) {
		t.Errorf("overlong caller deadline produced invalid request deadline %s", overlong)
	}
	if difference := overlong.Sub(absent); difference < -5*time.Second ||
		difference > 5*time.Second {
		t.Errorf("absent and overlong deadlines differ: %s versus %s", absent, overlong)
	}

	shortDeadline := time.Now().Add(2 * time.Second)
	shortCtx, cancelShort := context.WithDeadline(t.Context(), shortDeadline)
	defer cancelShort()
	carried, carriedOK := requestDeadline(shortCtx, t)
	if !carriedOK || !carried.Equal(shortDeadline) {
		t.Errorf("short caller deadline %s was replaced by %s", shortDeadline, carried)
	}
}

// An empty page after 50 full pages confirms completion at the bound.
func TestPagedListingEndingExactlyOnTheBoundSucceeds(t *testing.T) {
	t.Parallel()

	script := make([]scriptedAnswer, 0, 51)
	for range 50 {
		script = append(script, scriptedAnswer{body: volumeListPageBody()})
	}
	script = append(script, scriptedAnswer{body: `{"volumes":[]}`})

	transport := newStubTransport(script...)
	client := newStubClient(t, stubConfig(), transport)

	var volumes []evs.Volume
	err := mustFinish(t, 10*time.Second, func() error {
		var listErr error
		volumes, listErr = client.ListVolumes(t.Context(), evs.ListVolumeOpts{})
		return listErr
	})
	if err != nil {
		t.Fatalf("a listing that ends exactly on the page bound must succeed, got: %v", err)
	}
	if len(volumes) != 50 {
		t.Fatalf("expected 50 volumes from 50 pages, got %d", len(volumes))
	}
}

func TestCallerCancellationAbandonsARequestOnAnOpenConnection(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	client := newSocketClient(t, stubConfig(), server.URL)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	failed := make(chan error, 1)
	go func() {
		_, err := client.GetVolume(ctx, stubVolumeID)
		failed <- err
	}()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the server, so nothing was in flight to abandon")
	}

	cancel()

	select {
	case err := <-failed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation of the in-flight request, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller cancellation did not abandon the request waiting on the connection")
	}
}

func TestEveryPollingLoopTerminates(t *testing.T) {
	cases := []struct {
		name         string
		script       []scriptedAnswer
		invoke       func(context.Context, *evs.Client) error
		wantErr      error
		wantRequests []string
		// unbounded omits the caller deadline.
		unbounded bool
	}{
		{
			name: "volume job wait",
			script: []scriptedAnswer{
				{body: acceptedJobBody},
				{body: runningJobBody},
			},
			invoke: func(ctx context.Context, client *evs.Client) error {
				_, err := client.CreateVolume(ctx, evs.CreateVolumeOpts{
					AvailabilityZone: "eu-de-01",
					VolumeType:       "SSD",
					Size:             10,
				})
				return err
			},
			wantErr:      context.DeadlineExceeded,
			wantRequests: []string{"POST /cloudvolumes", "GET /jobs/", "GET /jobs/"},
		},
		{
			name: "attachment job wait",
			script: []scriptedAnswer{
				{body: noAttachmentsBody},
				{body: acceptedJobBody},
				{body: runningJobBody},
			},
			invoke: func(ctx context.Context, client *evs.Client) error {
				_, err := client.AttachVolume(ctx, stubVolumeID, stubServerID)
				return err
			},
			wantErr: context.DeadlineExceeded,
			wantRequests: []string{
				"GET /block_device", "POST /attachvolume", "GET /jobs/", "GET /jobs/",
			},
		},
		{
			name: "attachment state poll",
			script: []scriptedAnswer{
				{body: attachmentsBody("")},
			},
			invoke: func(ctx context.Context, client *evs.Client) error {
				_, err := client.AttachVolume(ctx, stubVolumeID, stubServerID)
				return err
			},
			wantErr:      context.DeadlineExceeded,
			wantRequests: []string{"GET /block_device", "GET /block_device"},
		},
		{
			name: "deletion absence wait",
			script: []scriptedAnswer{
				{body: volumeDetailBody("available")},
				{status: http.StatusAccepted},
				{body: volumeDetailBody("deleting")},
			},
			invoke: func(ctx context.Context, client *evs.Client) error {
				return client.DeleteVolume(ctx, stubVolumeID)
			},
			wantErr: context.DeadlineExceeded,
			wantRequests: []string{
				"GET " + stubVolumeID,
				"DELETE " + stubVolumeID,
				"GET " + stubVolumeID,
				"GET " + stubVolumeID,
			},
		},
		{
			name: "paged listing",
			script: []scriptedAnswer{
				{body: volumeListPageBody()},
			},
			invoke: func(ctx context.Context, client *evs.Client) error {
				_, err := client.ListVolumes(ctx, evs.ListVolumeOpts{})
				return err
			},
			wantErr:      evs.ErrOperationFailed,
			wantRequests: []string{"GET /cloudvolumes/detail", "GET /cloudvolumes/detail"},
			unbounded:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newStubTransport(tc.script...)
			client := newStubClient(t, stubConfig(), transport)

			ctx := t.Context()
			if !tc.unbounded {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 1500*time.Millisecond)
				defer cancel()
			}

			err := mustFinish(t, 10*time.Second, func() error {
				return tc.invoke(ctx, client)
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got: %v", tc.wantErr, err)
			}

			assertRequests(t, transport, tc.wantRequests...)
		})
	}
}
