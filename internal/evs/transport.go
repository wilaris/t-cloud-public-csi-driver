package evs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
)

const (
	// requestTimeout bounds one HTTP request end to end, including the response body. It is set on
	// the provider's HTTP client, so it also bounds the startup authentication exchange, which has
	// no caller context to inherit.
	requestTimeout = 60 * time.Second
	// maxOperationTimeout bounds one exported operation in total, including every request, job
	// wait and state poll it performs. A shorter caller deadline is preserved.
	maxOperationTimeout = 10 * time.Minute
	maxRedirects        = 3
)

// errGatewayFailure prevents SDK retries of potentially non-idempotent requests.
var errGatewayFailure = errors.New("cloud gateway reported a transient failure")

// errRedirectRefused lets sanitizeError recognize rejected redirects.
var errRedirectRefused = errors.New("refused a cloud redirect")

// contextTransport applies the caller context while preserving the provider's base transport
// and shared connection pool.
type contextTransport struct {
	ctx  context.Context
	next http.RoundTripper
}

// RoundTrip applies the caller context.
func (t *contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	ctx := t.ctx
	if ctx == nil {
		// Guard against a nil context: req.Clone would panic.
		ctx = context.Background()
	}
	return next.RoundTrip(req.Clone(ctx))
}

// gatewayTransport converts gateway responses before the SDK can retry them.
type gatewayTransport struct {
	next http.RoundTripper
}

// RoundTrip turns 502 and 504 responses into transport errors before the SDK sees them. The SDK
// otherwise retries these statuses automatically, including for non-idempotent requests such as
// volume creation, which could repeat an effect the cloud already accepted.
func (t *gatewayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}

	resp, err := next.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: status %d", errGatewayFailure, resp.StatusCode)
	}
	return resp, nil
}

// withRequestContext shallow-copies the clients and applies ctx to the shared base transport.
// AK/SK clients have no mutable token state after startup.
func withRequestContext(
	ctx context.Context,
	client *golangsdk.ServiceClient,
) *golangsdk.ServiceClient {
	if client == nil || client.ProviderClient == nil {
		return client
	}

	base := client.HTTPClient.Transport
	if _, ok := base.(*gatewayTransport); !ok {
		base = &gatewayTransport{next: base}
	}

	provider := *client.ProviderClient
	provider.HTTPClient.Transport = &contextTransport{
		ctx:  ctx,
		next: base,
	}

	scoped := *client
	scoped.ProviderClient = &provider
	return &scoped
}

// v3 returns the block storage v3 client bound to ctx.
func (c *Client) v3(ctx context.Context) *golangsdk.ServiceClient {
	return withRequestContext(ctx, c.v3Client)
}

// v2 returns the block storage v2 client bound to ctx.
func (c *Client) v2(ctx context.Context) *golangsdk.ServiceClient {
	return withRequestContext(ctx, c.v2Client)
}

// ecs returns the compute v1 client bound to ctx.
func (c *Client) ecs(ctx context.Context) *golangsdk.ServiceClient {
	return withRequestContext(ctx, c.ecsClient)
}

// checkRedirect accepts bounded same-origin hops and re-signs them. Cross-origin hops are rejected
// to protect credentials. The sentinel keeps rejected URLs out of returned errors.
func checkRedirect(cfg Config) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf(
				"%w: the redirect bound of %d was reached",
				errRedirectRefused,
				maxRedirects,
			)
		}

		previous := via[len(via)-1]
		if req.URL.Scheme != previous.URL.Scheme || req.URL.Host != previous.URL.Host {
			return fmt.Errorf(
				"%w: the hop leaves the configured cloud endpoint",
				errRedirectRefused,
			)
		}

		// ReSign re-applies the AK/SK signature for the new hop. With temporary credentials the
		// X-Security-Token header is not re-attached here; the http.Client's same-origin header
		// copy carries it over from the previous request.
		golangsdk.ReSign(req, golangsdk.SignOptions{
			AccessKey: cfg.AccessKey,
			SecretKey: cfg.SecretKey,
		})
		return nil
	}
}
