package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// metadataURL is the link-local instance metadata document.
	metadataURL = "http://169.254.169.254/openstack/latest/meta_data.json"
	// metadataTimeout covers the whole retrieval, including retries and backoff waits.
	metadataTimeout = 30 * time.Second
	// metadataAttemptTimeout is the per-attempt connect/read timeout.
	metadataAttemptTimeout = 5 * time.Second
	// metadataRetryBase and metadataRetryMax bound the wait between attempts. The service answers
	// in milliseconds when it is up, so the first retry is quick and the growth is capped.
	metadataRetryBase = 250 * time.Millisecond
	metadataRetryMax  = 2 * time.Second
	// metadataBodyLimit bounds how much of the response is read before decoding.
	metadataBodyLimit = 64 << 10
)

// nodeFacts holds the compute instance identity the node role reports to the container orchestrator.
type nodeFacts struct {
	serverUUID       string
	availabilityZone string
}

// metadataClient retrieves node facts from the fixed metadata endpoint. Retrieval happens once at
// startup; nothing here runs while the driver is serving.
type metadataClient struct {
	baseURL    string
	httpClient *http.Client
}

// newMetadataClient pins the fixed metadata URL.
// Transport disables proxies (link-local) and does not follow redirects.
func newMetadataClient() *metadataClient {
	return &metadataClient{
		baseURL: metadataURL,
		httpClient: &http.Client{
			Timeout:   metadataAttemptTimeout,
			Transport: &http.Transport{Proxy: nil},
			// Do not follow redirects; ErrUseLastResponse keeps Location out of the error.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// fetch returns server UUID and availability zone from the metadata document.
// Redirect, non-OK status, oversized body or bad JSON is an error.
//
// Transport failures and 5xx responses are retried with backoff until the overall deadline.
// Other failures are terminal. Backoff waits respect ctx cancellation.
func (c *metadataClient) fetch(ctx context.Context) (nodeFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	backoff := metadataRetryBase
	for {
		facts, retryable, err := c.attempt(ctx)
		if err == nil {
			return facts, nil
		}
		if !retryable {
			return nodeFacts{}, err
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			// Report why retrying stopped, keeping the answer that prompted it.
			return nodeFacts{}, fmt.Errorf("%w: %w", err, ctx.Err())
		case <-timer.C:
		}

		backoff = min(backoff*2, metadataRetryMax)
	}
}

// attempt performs one metadata request. It reports whether the failure is worth repeating.
func (c *metadataClient) attempt(ctx context.Context) (facts nodeFacts, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nodeFacts{}, false, fmt.Errorf("failed to build node metadata request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A transport failure is the unreachable-service case, unless the caller is done.
		return nodeFacts{}, ctx.Err() == nil, fmt.Errorf(
			"failed to retrieve node metadata: %w",
			err,
		)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Retry 5xx; treat other non-OK statuses as terminal.
		retryable := resp.StatusCode >= http.StatusInternalServerError
		return nodeFacts{}, retryable, fmt.Errorf(
			"node metadata request returned status %d",
			resp.StatusCode,
		)
	}

	var doc struct {
		UUID             string `json:"uuid"`
		AvailabilityZone string `json:"availability_zone"`
	}

	decoder := json.NewDecoder(io.LimitReader(resp.Body, metadataBodyLimit))
	if err := decoder.Decode(&doc); err != nil {
		return nodeFacts{}, false, fmt.Errorf("failed to decode node metadata: %w", err)
	}

	return nodeFacts{
		serverUUID:       strings.TrimSpace(doc.UUID),
		availabilityZone: strings.TrimSpace(doc.AvailabilityZone),
	}, false, nil
}
