package e2e

import (
	"context"
	"net/http"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
)

type requestContextTransport struct {
	ctx  context.Context
	next http.RoundTripper
}

func (t *requestContextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(req.Clone(t.ctx))
}

// serviceClientWithContext binds SDK requests to ctx without mutating the shared provider client.
func serviceClientWithContext(
	ctx context.Context,
	client *golangsdk.ServiceClient,
) *golangsdk.ServiceClient {
	if client == nil || client.ProviderClient == nil {
		return client
	}
	provider := *client.ProviderClient
	provider.HTTPClient.Transport = &requestContextTransport{
		ctx:  ctx,
		next: client.HTTPClient.Transport,
	}
	scoped := *client
	scoped.ProviderClient = &provider
	return &scoped
}
