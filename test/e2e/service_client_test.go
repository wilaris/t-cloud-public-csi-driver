package e2e

import (
	"context"
	"errors"
	"net/http"
	"testing"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
)

type cancellationTransport struct{}

func (cancellationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestServiceClientWithContextCancelsADirectRequest(t *testing.T) {
	provider := new(golangsdk.ProviderClient)
	provider.HTTPClient.Transport = cancellationTransport{}
	client := &golangsdk.ServiceClient{
		ProviderClient: provider,
		Endpoint:       "https://example.invalid/",
	}
	ctx, cancel := context.WithCancel(t.Context())
	scoped := serviceClientWithContext(ctx, client)
	cancel()

	_, err := scoped.Request(
		http.MethodDelete,
		scoped.ServiceURL("resource"),
		&golangsdk.RequestOpts{},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("request = %v, want context cancellation", err)
	}
	if client.HTTPClient.Transport == nil {
		t.Fatal("the shared provider transport was mutated")
	}
}
