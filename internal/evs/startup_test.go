package evs_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

// These tests exercise startup redirect and transient-status handling with status-only servers.

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

			failed := make(chan error, 1)
			go func() {
				_, err := evs.NewProviderClient(cfg)
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

	_, err := evs.NewProviderClient(cfg)
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
