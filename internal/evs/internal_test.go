package evs

import (
	"errors"
	"net/http"
	"testing"
)

func TestJobStatusEndpoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		endpoint string
		want     string
		wantErr  bool
	}{
		{
			name:     "a v3 endpoint is rewritten to v1",
			endpoint: "https://evs.example.invalid/v3/project/",
			want:     "https://evs.example.invalid/v1/project/",
		},
		{
			name:     "a v3 in the host name is left untouched",
			endpoint: "https://v3.evs.example.invalid/v3/project/",
			want:     "https://v3.evs.example.invalid/v1/project/",
		},
		{
			name:     "an endpoint without a v3 path segment is rejected",
			endpoint: "https://evs.example.invalid/v2/project/",
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := jobStatusEndpoint(tc.endpoint)
			if tc.wantErr {
				if !errors.Is(err, ErrOperationFailed) {
					t.Fatalf("expected a terminal failure, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("jobStatusEndpoint(%q) = %q, want %q", tc.endpoint, got, tc.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestContextTransportToleratesANilContext(t *testing.T) {
	t.Parallel()

	transport := &contextTransport{
		next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       http.NoBody,
				Request:    req,
			}, nil
		}),
	}

	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"https://evs.example.invalid/",
		nil,
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	// A nil transport context must not panic in req.Clone.
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("a nil transport context must not fail the round trip: %v", err)
	}
	_ = resp.Body.Close()
}
