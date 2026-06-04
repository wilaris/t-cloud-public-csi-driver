// Package evs provides OpenStack EVS authentication and client management.
package evs

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack"

	"wilaris.dev/t-cloud-public-csi-drive/internal/log"
)

const (
	EnvAuthURL       = "OS_AUTH_URL"
	EnvAccessKey     = "OS_ACCESS_KEY"
	EnvSecretKey     = "OS_SECRET_KEY" //nolint:gosec // environment variable name constant
	EnvProjectID     = "OS_PROJECT_ID"
	EnvRegionName    = "OS_REGION_NAME"
	EnvSecurityToken = "OS_SECURITY_TOKEN" //nolint:gosec // environment variable name constant
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
