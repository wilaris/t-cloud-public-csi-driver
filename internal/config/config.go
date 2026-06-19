// Package config provides runtime configuration parsing and validation for the CSI driver.
package config

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/version"
)

const (
	// DefaultDriverName is the default Container Storage Interface plugin identifier.
	DefaultDriverName = "evs.csi.t-cloud.wilaris.dev"
	// DefaultEndpoint is the default unix domain socket path for gRPC. The directory segment
	// must match DefaultDriverName, because kubelet derives the plugin socket directory from
	// the name the driver registers under.
	DefaultEndpoint = "unix:///var/lib/kubelet/plugins/evs.csi.t-cloud.wilaris.dev/csi.sock"

	EnvAuthURL       = "OS_AUTH_URL"
	EnvAccessKey     = "OS_ACCESS_KEY"
	EnvSecretKey     = "OS_SECRET_KEY" //nolint:gosec // environment variable name constant
	EnvProjectID     = "OS_PROJECT_ID"
	EnvRegionName    = "OS_REGION_NAME"
	EnvSecurityToken = "OS_SECURITY_TOKEN" //nolint:gosec // environment variable name constant
)

// DriverVersion is the release version reported through the CSI Identity service. The linker stamps
// it into the binary at build time; see the Makefile.
var DriverVersion = version.Version()

// ErrVersionRequested reports that --version was handled and the process should exit cleanly.
var ErrVersionRequested = errors.New("version requested")

// Role selects which CSI services a process registers and which inputs it requires.
type Role string

const (
	// RoleController serves the CSI Identity and Controller services and manages cloud volumes.
	RoleController Role = "controller"
	// RoleNode serves the CSI Identity and Node services and performs node-local storage work.
	RoleNode Role = "node"
)

// SecretString is a credential that redacts under fmt.
type SecretString string

// String returns "***" when non-empty so fmt does not print the secret.
func (s SecretString) String() string {
	if s == "" {
		return ""
	}
	return "***"
}

// Value returns the raw secret.
func (s SecretString) Value() string {
	return string(s)
}

// Config is validated operator input for one driver process.
type Config struct {
	// Role selects the CSI services this process registers and the inputs it requires.
	Role Role

	// CLI Flags
	Endpoint   string
	NodeID     string
	DriverName string
	// Version is the stamped build identity carried here for the Identity service to report.
	Version string
	// AvailabilityZone is the EVS availability zone of this node. It is required by the
	// Node service, which reports it as the node's zone topology, and unused by the
	// Controller service, which takes the zone from each request instead.
	AvailabilityZone string

	// Authentication environment variables. Populated for RoleController only.
	AuthURL       string
	AccessKey     SecretString
	SecretKey     SecretString
	ProjectID     string
	RegionName    string
	SecurityToken SecretString
}

// String formats Config with secrets redacted.
func (c Config) String() string {
	return fmt.Sprintf(
		"Config{Role:%q, Endpoint:%q, NodeID:%q, DriverName:%q, Version:%q, AvailabilityZone:%q, AuthURL:%q, AccessKey:%s, SecretKey:%s, ProjectID:%q, RegionName:%q, SecurityToken:%s}",
		c.Role,
		c.Endpoint,
		c.NodeID,
		c.DriverName,
		c.Version,
		c.AvailabilityZone,
		c.AuthURL,
		c.AccessKey,
		c.SecretKey,
		c.ProjectID,
		c.RegionName,
		c.SecurityToken,
	)
}

// LoadFromEnvAndFlags loads config from the process environment and args.
func LoadFromEnvAndFlags(ctx context.Context, args []string) (*Config, error) {
	return Load(ctx, args, os.Getenv)
}

// Load parses inputs, fills node facts when needed, and validates the selected role.
func Load(ctx context.Context, args []string, getenv func(string) string) (*Config, error) {
	cfg, err := Parse(args, getenv)
	if err != nil {
		return nil, err
	}

	if err := cfg.ResolveNodeFacts(ctx); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Parse reads flags and env via getenv. It does not call the metadata service;
// ResolveNodeFacts does that for the node role.
func Parse(args []string, getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	fs := flag.NewFlagSet("t-cloud-csi-driver", flag.ContinueOnError)

	// Parse failures return only via the error; usage is printed only for -h/--help.
	var flagOutput bytes.Buffer
	fs.SetOutput(&flagOutput)

	var (
		role             string
		endpoint         string
		nodeID           string
		showVersion      bool
		availabilityZone string
	)

	fs.StringVar(
		&role,
		"role",
		"",
		"driver role to run, either controller or node (required)",
	)
	fs.StringVar(
		&endpoint,
		"endpoint",
		DefaultEndpoint,
		"CSI endpoint socket path (unix:// or tcp://)",
	)
	fs.StringVar(
		&nodeID,
		"nodeid",
		"",
		"T Cloud Public compute instance Server UUID; overrides the metadata value on the node role",
	)
	fs.BoolVar(&showVersion, "version", false, "print the build identity and exit")
	fs.StringVar(
		&availabilityZone,
		"availability-zone",
		"",
		"T Cloud Public availability zone of this node; overrides the metadata value on the node role",
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = os.Stderr.Write(flagOutput.Bytes())
		}
		return nil, fmt.Errorf("failed to parse CLI flags: %w", err)
	}

	// Handled before the role check so the build identity is reachable without operator input.
	if showVersion {
		_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", fs.Name(), version.Get())
		return nil, ErrVersionRequested
	}

	parsedRole, err := parseRole(strings.TrimSpace(role))
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Role:             parsedRole,
		Endpoint:         strings.TrimSpace(endpoint),
		NodeID:           strings.TrimSpace(nodeID),
		DriverName:       DefaultDriverName,
		Version:          DriverVersion,
		AvailabilityZone: strings.TrimSpace(availabilityZone),
	}

	// Controller-only credentials: node never loads them.
	if parsedRole == RoleController {
		cfg.AuthURL = strings.TrimSpace(getenv(EnvAuthURL))
		cfg.AccessKey = SecretString(strings.TrimSpace(getenv(EnvAccessKey)))
		cfg.SecretKey = SecretString(strings.TrimSpace(getenv(EnvSecretKey)))
		cfg.ProjectID = strings.TrimSpace(getenv(EnvProjectID))
		cfg.RegionName = strings.TrimSpace(getenv(EnvRegionName))
		cfg.SecurityToken = SecretString(strings.TrimSpace(getenv(EnvSecurityToken)))
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// parseRole accepts controller or node.
func parseRole(raw string) (Role, error) {
	switch Role(raw) {
	case RoleController, RoleNode:
		return Role(raw), nil
	case "":
		return "", fmt.Errorf(
			"invalid configuration: missing required --role flag, expected %q or %q",
			RoleController,
			RoleNode,
		)
	default:
		return "", fmt.Errorf(
			"invalid configuration: unknown --role %q, expected %q or %q",
			raw,
			RoleController,
			RoleNode,
		)
	}
}

// Validate checks inputs required by the selected role.
// Node ID and zone are checked in ResolveNodeFacts (they may come from metadata).
func (c *Config) Validate() error {
	if c.Role != RoleController && c.Role != RoleNode {
		return fmt.Errorf(
			"invalid configuration: role must be %q or %q",
			RoleController,
			RoleNode,
		)
	}

	if _, _, err := resolveEndpoint(c.Endpoint); err != nil {
		return err
	}

	if c.DriverName == "" {
		return fmt.Errorf("invalid configuration: driver name cannot be empty")
	}

	if c.Version == "" {
		return fmt.Errorf("invalid configuration: version cannot be empty")
	}

	if c.Role == RoleController {
		return c.validateCloudCredentials()
	}

	return nil
}

// Network returns the network and address to listen on.
func (c *Config) Network() (network, address string, err error) {
	return resolveEndpoint(c.Endpoint)
}

// resolveEndpoint converts an endpoint URI into the net.Listen network and address.
// Query, fragment, userinfo and other parts the listener would ignore are rejected.
func resolveEndpoint(endpoint string) (network, address string, err error) {
	if endpoint == "" {
		return "", "", fmt.Errorf("invalid configuration: endpoint cannot be empty")
	}

	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || (u.Scheme != "unix" && u.Scheme != "tcp") {
		return "", "", fmt.Errorf(
			"invalid configuration: endpoint must be a valid unix:// or tcp:// URI, got %q",
			endpoint,
		)
	}

	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", "", fmt.Errorf(
			"invalid configuration: endpoint must carry no query, fragment or user information, got %q",
			endpoint,
		)
	}

	if u.Scheme == "tcp" {
		// tcp without a host would bind all interfaces on a random port.
		if u.Host == "" {
			return "", "", fmt.Errorf(
				"invalid configuration: tcp endpoint must name a host address, got %q",
				endpoint,
			)
		}
		if u.Path != "" {
			return "", "", fmt.Errorf(
				"invalid configuration: tcp endpoint must carry no path, got %q",
				endpoint,
			)
		}
		return "tcp", u.Host, nil
	}

	// Missing third slash on unix:// puts the path in Host; reject that form.
	if u.Host != "" {
		return "", "", fmt.Errorf(
			"invalid configuration: unix endpoint must use the unix:///absolute/path form, got %q",
			endpoint,
		)
	}

	// listen creates the parent dir and may remove a stale socket; require a clean abs path first.
	if !isCleanAbsPath(u.Path) {
		return "", "", fmt.Errorf(
			"invalid configuration: unix endpoint must be an absolute, clean socket path, got %q",
			endpoint,
		)
	}

	return "unix", u.Path, nil
}

// isCleanAbsPath reports whether path is absolute and filepath.Clean-stable.
func isCleanAbsPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// validateCloudCredentials requires controller AK/SK fields. SecurityToken is optional.
func (c *Config) validateCloudCredentials() error {
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

// ResolveNodeFacts sets node Server UUID and availability zone.
// Explicit flags win per field; metadata fills the rest. Skips the metadata call when both
// are already set. No-op for controller.
func (c *Config) ResolveNodeFacts(ctx context.Context) error {
	return c.resolveNodeFacts(ctx, newMetadataClient())
}

func (c *Config) resolveNodeFacts(ctx context.Context, metadata *metadataClient) error {
	if c.Role != RoleNode {
		return nil
	}

	if c.NodeID != "" && c.AvailabilityZone != "" {
		return c.validateNodeFacts()
	}

	facts, err := metadata.fetch(ctx)
	if err != nil {
		return err
	}

	if c.NodeID == "" {
		c.NodeID = facts.serverUUID
	}
	if c.AvailabilityZone == "" {
		c.AvailabilityZone = facts.availabilityZone
	}

	return c.validateNodeFacts()
}

// validateNodeFacts requires NodeID and AvailabilityZone for the node role.
func (c *Config) validateNodeFacts() error {
	if c.NodeID == "" {
		return fmt.Errorf(
			"invalid configuration: node ID is required; pass --nodeid or make the node metadata service reachable",
		)
	}

	if c.AvailabilityZone == "" {
		return fmt.Errorf(
			"invalid configuration: availability zone is required; pass --availability-zone or make the node metadata service reachable",
		)
	}

	return nil
}
