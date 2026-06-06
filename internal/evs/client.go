// Package evs provides OpenStack EVS authentication and client management.
package evs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack"

	"wilaris.dev/t-cloud-public-csi-driver/internal/log"
)

const (
	EnvAuthURL       = "OS_AUTH_URL"
	EnvAccessKey     = "OS_ACCESS_KEY"
	EnvSecretKey     = "OS_SECRET_KEY" //nolint:gosec // environment variable name constant
	EnvProjectID     = "OS_PROJECT_ID"
	EnvRegionName    = "OS_REGION_NAME"
	EnvSecurityToken = "OS_SECURITY_TOKEN" //nolint:gosec // environment variable name constant
)

// Package-level error definitions for domain-level EVS operation classification.
var (
	// ErrNotFound indicates the target cloud resource was not found.
	ErrNotFound = errors.New("cloud resource not found")
	// ErrConflict indicates a state conflict during a cloud operation.
	ErrConflict = errors.New("cloud operation conflict")
	// ErrInvalidArgument indicates invalid or rejected operation inputs.
	ErrInvalidArgument = errors.New("cloud rejected input")
	// ErrUnauthenticated indicates authentication failure with the cloud service.
	ErrUnauthenticated = errors.New("cloud authentication failed")
	// ErrPermissionDenied indicates authorization failure for the requested operation.
	ErrPermissionDenied = errors.New("cloud authorization failed")
	// ErrUnavailable indicates a transient or temporary cloud service unavailability.
	ErrUnavailable = errors.New("cloud service unavailable")
	// ErrOperationFailed indicates a non-transient error or unexpected failure in cloud operations.
	ErrOperationFailed = errors.New("cloud operation failed")
)

// Config holds validated AK/SK authentication credentials and target cloud settings.
type Config struct {
	AuthURL       string
	AccessKey     string
	SecretKey     string
	ProjectID     string
	RegionName    string
	SecurityToken string
}

// LoadConfigFromEnv reads and validates EVS credentials from process environment variables.
func LoadConfigFromEnv() (Config, error) {
	return LoadConfig(os.Getenv)
}

// LoadConfig reads and validates EVS credentials using a custom environment lookup function.
func LoadConfig(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg := Config{
		AuthURL:       strings.TrimSpace(getenv(EnvAuthURL)),
		AccessKey:     strings.TrimSpace(getenv(EnvAccessKey)),
		SecretKey:     strings.TrimSpace(getenv(EnvSecretKey)),
		ProjectID:     strings.TrimSpace(getenv(EnvProjectID)),
		RegionName:    strings.TrimSpace(getenv(EnvRegionName)),
		SecurityToken: strings.TrimSpace(getenv(EnvSecurityToken)),
	}

	if cfg.AuthURL == "" {
		return Config{}, fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvAuthURL,
		)
	}
	if cfg.AccessKey == "" {
		return Config{}, fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvAccessKey,
		)
	}
	if cfg.SecretKey == "" {
		return Config{}, fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvSecretKey,
		)
	}
	if cfg.ProjectID == "" {
		return Config{}, fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvProjectID,
		)
	}
	if cfg.RegionName == "" {
		return Config{}, fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvRegionName,
		)
	}

	return cfg, nil
}

// NewProviderClient constructs an authenticated, project- and region-scoped ProviderClient.
func NewProviderClient(cfg Config) (*golangsdk.ProviderClient, error) {
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

	client.HTTPClient = http.Client{
		Transport: client.HTTPClient.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			golangsdk.ReSign(req, golangsdk.SignOptions{
				AccessKey: cfg.AccessKey,
				SecretKey: cfg.SecretKey,
			})
			return nil
		},
	}

	if err := openstack.Authenticate(client, opts); err != nil {
		return nil, fmt.Errorf(
			"failed to authenticate provider client: %w",
			sanitizeError(err, cfg),
		)
	}

	return client, nil
}

// NewProviderClientFromEnv loads configuration from process environment and creates a ProviderClient.
func NewProviderClientFromEnv() (*golangsdk.ProviderClient, error) {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return NewProviderClient(cfg)
}

// sanitizeError removes credential values from error messages.
func sanitizeError(err error, cfg Config) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if cfg.AccessKey != "" {
		msg = strings.ReplaceAll(msg, cfg.AccessKey, "[REDACTED]")
	}
	if cfg.SecretKey != "" {
		msg = strings.ReplaceAll(msg, cfg.SecretKey, "[REDACTED]")
	}
	if cfg.SecurityToken != "" {
		msg = strings.ReplaceAll(msg, cfg.SecurityToken, "[REDACTED]")
	}
	msg = log.RedactString(msg)
	return fmt.Errorf("%s", msg)
}

// classifyError wraps an operation error with a domain sentinel error and redacts sensitive data.
func (c *Client) classifyError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}

	kind := classifyErrorKind(err)
	safeErr := sanitizeError(err, c.cfg)
	return fmt.Errorf("%s: %w: %s", operation, kind, safeErr)
}

// classifyErrorKind maps SDK and system errors to package sentinel error categories.
func classifyErrorKind(err error) error {
	var (
		badRequest     golangsdk.ErrDefault400
		unauthorized   golangsdk.ErrDefault401
		forbidden      golangsdk.ErrDefault403
		notFound       golangsdk.ErrDefault404
		requestTimeout golangsdk.ErrDefault408
		conflict       golangsdk.ErrDefault409
		tooMany        golangsdk.ErrDefault429
		serverError    golangsdk.ErrDefault500
		unavailable    golangsdk.ErrDefault503
		responseError  golangsdk.ErrUnexpectedResponseCode
		networkError   net.Error
	)

	switch {
	case errors.Is(err, ErrNotFound),
		errors.Is(err, ErrConflict),
		errors.Is(err, ErrInvalidArgument),
		errors.Is(err, ErrUnauthenticated),
		errors.Is(err, ErrPermissionDenied),
		errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrOperationFailed):
		return err
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
	case errors.As(err, &requestTimeout),
		errors.As(err, &tooMany),
		errors.As(err, &serverError),
		errors.As(err, &unavailable),
		errors.As(err, &networkError):
		return ErrUnavailable
	case errors.As(err, &responseError):
		if responseError.Actual == http.StatusBadGateway ||
			responseError.Actual == http.StatusGatewayTimeout {
			return ErrUnavailable
		}
	}

	return ErrOperationFailed
}
