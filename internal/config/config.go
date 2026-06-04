// Package config provides runtime configuration parsing and validation for the CSI driver.
package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	// DefaultDriverName is the default Container Storage Interface plugin identifier.
	DefaultDriverName = "evs.csi.t-cloud.ti-services.io"
	// DefaultEndpoint is the default unix domain socket path for gRPC.
	DefaultEndpoint = "unix:///var/lib/kubelet/plugins/evs.csi.t-cloud.ti-services.io/csi.sock"
	// DefaultVersion is the default driver release version.
	DefaultVersion = "v0.1.0"

	EnvAuthURL       = "OS_AUTH_URL"
	EnvAccessKey     = "OS_ACCESS_KEY"
	EnvSecretKey     = "OS_SECRET_KEY" //nolint:gosec // environment variable name constant
	EnvProjectID     = "OS_PROJECT_ID"
	EnvRegionName    = "OS_REGION_NAME"
	EnvSecurityToken = "OS_SECURITY_TOKEN" //nolint:gosec // environment variable name constant
)

// SecretString wraps a sensitive credential string and implements fmt.Stringer to prevent credential exposure.
type SecretString string

// String returns redacted representation of secret string or empty string if empty.
func (s SecretString) String() string {
	if s == "" {
		return ""
	}
	return "***"
}

// Value returns the raw unredacted secret string value.
func (s SecretString) Value() string {
	return string(s)
}

// Config represents runtime configuration for the CSI driver.
type Config struct {
	// CLI Flags
	Endpoint   string
	NodeID     string
	DriverName string
	Version    string

	// Authentication Environment Variables
	AuthURL       string
	AccessKey     SecretString
	SecretKey     SecretString
	ProjectID     string
	RegionName    string
	SecurityToken SecretString
}

// String implements fmt.Stringer to produce a safe string representation of Config without leaking credentials.
func (c Config) String() string {
	return fmt.Sprintf(
		"Config{Endpoint:%q, NodeID:%q, DriverName:%q, Version:%q, AuthURL:%q, AccessKey:%s, SecretKey:%s, ProjectID:%q, RegionName:%q, SecurityToken:%s}",
		c.Endpoint,
		c.NodeID,
		c.DriverName,
		c.Version,
		c.AuthURL,
		c.AccessKey,
		c.SecretKey,
		c.ProjectID,
		c.RegionName,
		c.SecurityToken,
	)
}

// LoadFromEnvAndFlags parses CLI flags and environment variables using process environment and arguments.
func LoadFromEnvAndFlags(args []string) (*Config, error) {
	return Parse(args, os.Getenv)
}

// Parse parses CLI flags and reads environment variables using a custom environment lookup function.
func Parse(args []string, getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	fs := flag.NewFlagSet("t-cloud-csi-driver", flag.ContinueOnError)

	var (
		endpoint   string
		nodeID     string
		driverName string
		version    string
	)

	fs.StringVar(
		&endpoint,
		"endpoint",
		DefaultEndpoint,
		"CSI endpoint socket path (unix:// or tcp://)",
	)
	fs.StringVar(&nodeID, "nodeid", "", "T Cloud Public compute instance Server UUID (required)")
	fs.StringVar(&driverName, "driver-name", DefaultDriverName, "CSI driver name")
	fs.StringVar(&version, "version", DefaultVersion, "CSI driver version")

	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("failed to parse CLI flags: %w", err)
	}

	cfg := &Config{
		Endpoint:      strings.TrimSpace(endpoint),
		NodeID:        strings.TrimSpace(nodeID),
		DriverName:    strings.TrimSpace(driverName),
		Version:       strings.TrimSpace(version),
		AuthURL:       strings.TrimSpace(getenv(EnvAuthURL)),
		AccessKey:     SecretString(strings.TrimSpace(getenv(EnvAccessKey))),
		SecretKey:     SecretString(strings.TrimSpace(getenv(EnvSecretKey))),
		ProjectID:     strings.TrimSpace(getenv(EnvProjectID)),
		RegionName:    strings.TrimSpace(getenv(EnvRegionName)),
		SecurityToken: SecretString(strings.TrimSpace(getenv(EnvSecurityToken))),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate performs single-pass ingress validation on all configuration parameters.
func (c *Config) Validate() error {
	if c.Endpoint == "" {
		return fmt.Errorf("invalid configuration: endpoint cannot be empty")
	}

	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme == "" || (u.Scheme != "unix" && u.Scheme != "tcp") {
		return fmt.Errorf(
			"invalid configuration: endpoint must be a valid unix:// or tcp:// URI, got %q",
			c.Endpoint,
		)
	}

	if c.NodeID == "" {
		return fmt.Errorf("invalid configuration: missing required --nodeid flag")
	}

	if c.DriverName == "" {
		return fmt.Errorf("invalid configuration: driver-name cannot be empty")
	}

	if c.Version == "" {
		return fmt.Errorf("invalid configuration: version cannot be empty")
	}

	if c.AuthURL == "" {
		return fmt.Errorf("missing or empty required environment variable %s", EnvAuthURL)
	}

	authURLParsed, err := url.Parse(c.AuthURL)
	if err != nil || authURLParsed.Scheme == "" || authURLParsed.Host == "" {
		return fmt.Errorf(
			"invalid environment variable %s: must be a valid URL with scheme and host",
			EnvAuthURL,
		)
	}

	if c.AccessKey.Value() == "" {
		return fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvAccessKey,
		)
	}

	if c.SecretKey.Value() == "" {
		return fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvSecretKey,
		)
	}

	if c.ProjectID == "" {
		return fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvProjectID,
		)
	}

	if c.RegionName == "" {
		return fmt.Errorf(
			"missing or empty required environment variable %s",
			EnvRegionName,
		)
	}

	return nil
}
