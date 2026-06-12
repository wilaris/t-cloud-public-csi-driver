// Package evs provides OpenStack EVS authentication and client management.
package evs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
)

var (
	// ErrNotFound indicates the target cloud resource was not found.
	ErrNotFound = errors.New("cloud resource not found")
	// ErrConflict indicates a state conflict during a cloud operation.
	ErrConflict = errors.New("cloud operation conflict")
	// ErrInvalidArgument indicates invalid or rejected operation inputs.
	ErrInvalidArgument = errors.New("cloud rejected input")
	// ErrNotOwned indicates the target volume does not carry this driver's ownership marker.
	ErrNotOwned = errors.New("volume is not owned by this driver")
	// ErrUnauthenticated indicates authentication failure with the cloud service.
	ErrUnauthenticated = errors.New("cloud authentication failed")
	// ErrPermissionDenied indicates authorization failure for the requested operation.
	ErrPermissionDenied = errors.New("cloud authorization failed")
	// ErrUnavailable indicates a transient cloud service failure.
	ErrUnavailable = errors.New("cloud service unavailable")
	// ErrOperationFailed indicates a non-transient error or unexpected failure in cloud operations.
	ErrOperationFailed = errors.New("cloud operation failed")
)

// Config is validated AK/SK auth and cloud target settings (caller validates).
type Config struct {
	AuthURL       string
	AccessKey     string
	SecretKey     string
	ProjectID     string
	RegionName    string
	SecurityToken string
}

// NewProviderClient constructs an authenticated, project- and region-scoped ProviderClient.
func NewProviderClient(ctx context.Context, cfg Config) (*golangsdk.ProviderClient, error) {
	if cfg.AuthURL == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.ProjectID == "" ||
		cfg.RegionName == "" {
		return nil, fmt.Errorf("invalid configuration: required fields missing")
	}

	opts := golangsdk.AKSKAuthOptions{
		IdentityEndpoint: cfg.AuthURL,
		AccessKey:        cfg.AccessKey,
		SecretKey:        cfg.SecretKey,
		ProjectId:        cfg.ProjectID,
		SecurityToken:    cfg.SecurityToken,
	}

	client, err := openstack.NewClient(cfg.AuthURL)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to create provider client: %w",
			sanitizeError(err, cfg),
		)
	}

	// Authenticate under the caller's context so shutdown can abort the exchange.
	gateway := &gatewayTransport{next: client.HTTPClient.Transport}
	client.HTTPClient = http.Client{
		Transport:     &contextTransport{ctx: ctx, next: gateway},
		Timeout:       requestTimeout,
		CheckRedirect: checkRedirect(cfg),
	}

	// Disable the SDK's shared backoff; the CSI sidecar handles transient retries.
	noBackoffRetries := 0
	backoffTimeout := time.Duration(0)
	client.MaxBackoffRetries = &noBackoffRetries
	client.BackoffRetryTimeout = &backoffTimeout

	if err := openstack.Authenticate(client, opts); err != nil {
		return nil, fmt.Errorf(
			"failed to authenticate provider client: %w",
			sanitizeError(err, cfg),
		)
	}

	// Clear the startup context from the shared provider so later calls use their own ctx.
	client.HTTPClient.Transport = gateway

	return client, nil
}

func sanitizeError(err error, cfg Config) error {
	if err == nil {
		return nil
	}
	// Avoid exposing a rejected redirect URL from url.Error.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && errors.Is(urlErr.Err, errRedirectRefused) {
		return urlErr.Err
	}
	// Redact longer secrets first to prevent partial replacement.
	secrets := make([]string, 0, 3)
	for _, secret := range []string{cfg.AccessKey, cfg.SecretKey, cfg.SecurityToken} {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	slices.SortFunc(secrets, func(a, b string) int {
		return len(b) - len(a)
	})

	msg := err.Error()
	for _, secret := range secrets {
		msg = strings.ReplaceAll(msg, secret, "[REDACTED]")
	}
	msg = log.RedactString(msg)
	return errors.New(msg)
}

// classifyError maps err to a domain sentinel and redacts secrets in the text.
func (c *Client) classifyError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w: %s", operation, context.Canceled, sanitizeError(err, c.cfg))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"%s: %w: %s",
			operation,
			context.DeadlineExceeded,
			sanitizeError(err, c.cfg),
		)
	}

	kind := classifyErrorKind(err)
	safeErr := sanitizeError(err, c.cfg)
	return fmt.Errorf("%s: %w: %s", operation, kind, safeErr)
}

// classifyErrorKind returns a sentinel only; it does not wrap the raw SDK error.
func classifyErrorKind(err error) error {
	var (
		badRequest    golangsdk.ErrDefault400
		unauthorized  golangsdk.ErrDefault401
		forbidden     golangsdk.ErrDefault403
		notFound      golangsdk.ErrDefault404
		timedOut      golangsdk.ErrDefault408
		conflict      golangsdk.ErrDefault409
		tooMany       golangsdk.ErrDefault429
		serverError   golangsdk.ErrDefault500
		unavailable   golangsdk.ErrDefault503
		responseError golangsdk.ErrUnexpectedResponseCode
		networkError  net.Error
	)

	for _, sentinel := range []error{
		ErrNotFound,
		ErrConflict,
		ErrInvalidArgument,
		ErrNotOwned,
		ErrUnauthenticated,
		ErrPermissionDenied,
		ErrUnavailable,
		ErrOperationFailed,
	} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}

	switch {
	case errors.Is(err, errGatewayFailure):
		return ErrUnavailable
	case errors.Is(err, errRedirectRefused):
		return ErrOperationFailed
	case errors.As(err, &badRequest):
		return ErrInvalidArgument
	case errors.As(err, &unauthorized):
		return ErrUnauthenticated
	case errors.As(err, &forbidden):
		return ErrPermissionDenied
	case errors.As(err, &notFound):
		return ErrNotFound
	case errors.As(err, &conflict):
		return ErrConflict
	case errors.As(err, &timedOut),
		errors.As(err, &tooMany),
		errors.As(err, &serverError),
		errors.As(err, &unavailable):
		return ErrUnavailable
	// Network timeouts are transient; other net.Error values (refused, DNS) are terminal.
	case errors.As(err, &networkError) && networkError.Timeout():
		return ErrUnavailable
	case errors.As(err, &responseError):
		if responseError.Actual == http.StatusBadGateway ||
			responseError.Actual == http.StatusGatewayTimeout {
			return ErrUnavailable
		}
	}

	return ErrOperationFailed
}
