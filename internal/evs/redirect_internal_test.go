package evs

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// redirectRequest builds a client request for target.
func redirectRequest(t *testing.T, target string) *http.Request {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", target, err)
	}
	return req
}

// Test checkRedirect directly; startup tests cover provider wiring.
func TestRedirectPolicy(t *testing.T) {
	t.Parallel()

	cfg := Config{AccessKey: "stub-access-key", SecretKey: "stub-secret-key"}
	policy := checkRedirect(cfg)

	const configured = "https://evs.example.invalid/v3/project/volumes"

	cases := []struct {
		name    string
		target  string
		hops    int
		refused bool
	}{
		{
			name:    "a hop that stays on the same scheme and host is re-signed",
			target:  configured + "/next",
			hops:    1,
			refused: false,
		},
		{
			name:    "the third same-origin hop is followed",
			target:  configured + "/next",
			hops:    3,
			refused: false,
		},
		{
			name:    "a hop to another host is refused",
			target:  "https://elsewhere.example.invalid/v3/project/volumes",
			hops:    1,
			refused: true,
		},
		{
			name:    "a hop to another scheme is refused",
			target:  "http://evs.example.invalid/v3/project/volumes",
			hops:    1,
			refused: true,
		},
		{
			name:    "a chain past the bound is refused",
			target:  configured + "/next",
			hops:    4,
			refused: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			via := make([]*http.Request, 0, tc.hops)
			for range tc.hops {
				via = append(via, redirectRequest(t, configured))
			}
			req := redirectRequest(t, tc.target)

			err := policy(req, via)

			if !tc.refused {
				if err != nil {
					t.Fatalf("expected the hop to be followed, got: %v", err)
				}
				if req.Header.Get("Authorization") == "" {
					t.Fatal("a same-origin hop was not re-signed")
				}
				return
			}

			if err == nil {
				t.Fatal("expected the hop to be refused")
			}
			kind := classifyErrorKind(&url.Error{Op: http.MethodGet, URL: tc.target, Err: err})
			if !errors.Is(kind, ErrOperationFailed) {
				t.Fatalf("redirect refusal classified as %v instead of terminal failure", kind)
			}
			if req.Header.Get("Authorization") != "" || req.Header.Get("X-Sdk-Date") != "" {
				t.Fatalf("a refused hop carried signing headers: %v", req.Header)
			}
			if strings.Contains(err.Error(), req.URL.Host) {
				t.Fatalf("redirect error exposes the target host: %v", err)
			}
			for _, secret := range []string{cfg.AccessKey, cfg.SecretKey} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("redirect error contains a credential: %v", err)
				}
			}
		})
	}
}
