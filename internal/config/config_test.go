package config_test

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"wilaris.dev/t-cloud-public-csi-driver/internal/version"
)

func validEnvMap() map[string]string {
	return map[string]string{
		config.EnvAuthURL:       "https://iam.eu-de.otc.t-systems.com/v3",
		config.EnvAccessKey:     "AKIAIOSFODNN7EXAMPLE",
		config.EnvSecretKey:     "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		config.EnvProjectID:     "1234567890abcdef1234567890abcdef",
		config.EnvRegionName:    "eu-de",
		config.EnvSecurityToken: "token-abc-123",
	}
}

func mockGetenv(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func TestParseSuccess(t *testing.T) {
	t.Parallel()

	env := validEnvMap()
	args := []string{
		"--role", "controller",
		"--nodeid", "68e1a123-4567-89ab-cdef-0123456789ab",
		"--endpoint", "unix:///tmp/csi.sock",
		"--driver-name", "my-custom-driver",
		"--availability-zone", "eu-de-01",
	}

	cfg, err := config.Parse(args, mockGetenv(env))
	if err != nil {
		t.Fatalf("expected clean parse, got error: %v", err)
	}

	if cfg.NodeID != "68e1a123-4567-89ab-cdef-0123456789ab" {
		t.Errorf("expected NodeID %q, got %q", "68e1a123-4567-89ab-cdef-0123456789ab", cfg.NodeID)
	}
	if cfg.Endpoint != "unix:///tmp/csi.sock" {
		t.Errorf("expected Endpoint %q, got %q", "unix:///tmp/csi.sock", cfg.Endpoint)
	}
	if cfg.AvailabilityZone != "eu-de-01" {
		t.Errorf("expected AvailabilityZone %q, got %q", "eu-de-01", cfg.AvailabilityZone)
	}
	if cfg.DriverName != "my-custom-driver" {
		t.Errorf("expected DriverName %q, got %q", "my-custom-driver", cfg.DriverName)
	}
	if cfg.Version != config.DriverVersion {
		t.Errorf("expected Version %q, got %q", config.DriverVersion, cfg.Version)
	}
	if cfg.AuthURL != env[config.EnvAuthURL] {
		t.Errorf("expected AuthURL %q, got %q", env[config.EnvAuthURL], cfg.AuthURL)
	}
	if cfg.AccessKey.Value() != env[config.EnvAccessKey] {
		t.Errorf(
			"expected AccessKey value %q, got %q",
			env[config.EnvAccessKey],
			cfg.AccessKey.Value(),
		)
	}
	if cfg.SecretKey.Value() != env[config.EnvSecretKey] {
		t.Errorf(
			"expected SecretKey value %q, got %q",
			env[config.EnvSecretKey],
			cfg.SecretKey.Value(),
		)
	}
	if cfg.ProjectID != env[config.EnvProjectID] {
		t.Errorf("expected ProjectID %q, got %q", env[config.EnvProjectID], cfg.ProjectID)
	}
	if cfg.RegionName != env[config.EnvRegionName] {
		t.Errorf("expected RegionName %q, got %q", env[config.EnvRegionName], cfg.RegionName)
	}
	if cfg.SecurityToken.Value() != env[config.EnvSecurityToken] {
		t.Errorf(
			"expected SecurityToken value %q, got %q",
			env[config.EnvSecurityToken],
			cfg.SecurityToken.Value(),
		)
	}
}

func TestParseDefaultFlags(t *testing.T) {
	t.Parallel()

	env := validEnvMap()
	args := []string{"--role", "controller", "--nodeid", "node-uuid-123"}

	cfg, err := config.Parse(args, mockGetenv(env))
	if err != nil {
		t.Fatalf("expected clean parse with defaults, got error: %v", err)
	}

	if cfg.Endpoint != config.DefaultEndpoint {
		t.Errorf("expected default Endpoint %q, got %q", config.DefaultEndpoint, cfg.Endpoint)
	}
	if cfg.DriverName != config.DefaultDriverName {
		t.Errorf("expected default DriverName %q, got %q", config.DefaultDriverName, cfg.DriverName)
	}
	if cfg.Version != config.DriverVersion {
		t.Errorf("expected stamped Version %q, got %q", config.DriverVersion, cfg.Version)
	}
}

func TestUnacceptedAuthMethodsAndAliasesNotSupported(t *testing.T) {
	t.Parallel()

	unacceptedEnv := map[string]string{
		"OS_USERNAME":    "test-user",
		"OS_PASSWORD":    "test-pass",
		"OS_TENANT_NAME": "test-tenant",
		"OS_DOMAIN_NAME": "test-domain",
	}
	args := []string{"--role", "controller"}

	cfg, err := config.Parse(args, mockGetenv(unacceptedEnv))
	if err == nil {
		t.Fatalf("expected parse to fail when required AK/SK vars are missing, got: %+v", cfg)
	}
}

func TestParseNodeRoleReadsNoCredentialVariable(t *testing.T) {
	t.Parallel()

	env := validEnvMap()
	var reads []string
	spy := func(key string) string {
		reads = append(reads, key)
		return env[key]
	}

	cfg, err := config.Parse([]string{
		"--role", "node",
		"--nodeid", "node-uuid-123",
		"--availability-zone", "eu-de-01",
	}, spy)
	if err != nil {
		t.Fatalf("expected the node role to parse without credentials, got: %v", err)
	}

	if cfg.AuthURL != "" || cfg.ProjectID != "" || cfg.RegionName != "" {
		t.Errorf("node configuration carries cloud settings: %s", cfg)
	}
	if cfg.AccessKey.Value() != "" || cfg.SecretKey.Value() != "" ||
		cfg.SecurityToken.Value() != "" {
		t.Error("node configuration carries a credential value")
	}

	credentialKeys := []string{
		config.EnvAuthURL,
		config.EnvAccessKey,
		config.EnvSecretKey,
		config.EnvProjectID,
		config.EnvRegionName,
		config.EnvSecurityToken,
	}
	for _, read := range reads {
		if slices.Contains(credentialKeys, read) {
			t.Errorf("node role read environment variable %s", read)
		}
	}
}

func TestInvalidEndpoint(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		endpoint string
	}{
		{"invalid scheme", "http://localhost/socket"},
		{"missing scheme", "/var/lib/kubelet/csi.sock"},
		{"empty endpoint", ""},
		{"unix endpoint with host segment", "unix://var/lib/kubelet/csi.sock"},
		{"unix endpoint with traversal", "unix:///var/lib/../csi.sock"},
		{"tcp endpoint without host", "tcp://"},
		{"tcp endpoint with path", "tcp://127.0.0.1:9000/csi"},
		{"unix endpoint with query", "unix:///run/csi.sock?mode=0600"},
		{"unix endpoint with fragment", "unix:///run/csi.sock#main"},
		{"unix endpoint with user information", "unix://user@/run/csi.sock"},
		{"tcp endpoint with user information", "tcp://user@127.0.0.1:9000"},
		{"tcp endpoint with query", "tcp://127.0.0.1:9000?tls=on"},
		{"tcp endpoint with fragment", "tcp://127.0.0.1:9000#main"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := validEnvMap()
			args := []string{
				"--role",
				"controller",
				"--nodeid",
				"node-uuid-123",
				"--endpoint",
				tc.endpoint,
			}

			_, err := config.Parse(args, mockGetenv(env))
			if err == nil {
				t.Fatalf("expected error for endpoint %q, got nil", tc.endpoint)
			}
		})
	}
}

func TestNetworkResolvesWhatTheListenerBinds(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		endpoint    string
		wantNetwork string
		wantAddress string
	}{
		{
			"unix endpoint",
			"unix:///var/lib/kubelet/plugins/csi.sock",
			"unix",
			"/var/lib/kubelet/plugins/csi.sock",
		},
		{
			"default endpoint",
			config.DefaultEndpoint,
			"unix",
			"/var/lib/kubelet/plugins/evs.csi.t-cloud.wilaris.dev/csi.sock",
		},
		{"tcp endpoint", "tcp://127.0.0.1:9000", "tcp", "127.0.0.1:9000"},
		{"tcp endpoint on all interfaces", "tcp://0.0.0.0:9000", "tcp", "0.0.0.0:9000"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := []string{"--role", "controller", "--endpoint", tc.endpoint}

			cfg, err := config.Parse(args, mockGetenv(validEnvMap()))
			if err != nil {
				t.Fatalf("expected endpoint %q to parse, got %v", tc.endpoint, err)
			}

			network, address, err := cfg.Network()
			if err != nil {
				t.Fatalf("Network() failed: %v", err)
			}
			if network != tc.wantNetwork {
				t.Errorf("expected network %q, got %q", tc.wantNetwork, network)
			}
			if address != tc.wantAddress {
				t.Errorf("expected address %q, got %q", tc.wantAddress, address)
			}
		})
	}
}

func TestMissingRequiredEnvVars(t *testing.T) {
	t.Parallel()

	requiredVars := []string{
		config.EnvAuthURL,
		config.EnvAccessKey,
		config.EnvSecretKey,
		config.EnvProjectID,
		config.EnvRegionName,
	}

	for _, reqVar := range requiredVars {
		t.Run("missing_"+reqVar, func(t *testing.T) {
			t.Parallel()
			env := validEnvMap()
			delete(env, reqVar)
			args := []string{"--role", "controller", "--nodeid", "node-uuid-123"}

			_, err := config.Parse(args, mockGetenv(env))
			if err == nil {
				t.Fatalf("expected error when %s is missing, got nil", reqVar)
			}
			if !strings.Contains(err.Error(), reqVar) {
				t.Errorf("expected error message to mention %s, got: %v", reqVar, err)
			}
		})
	}
}

func TestInvalidAuthURL(t *testing.T) {
	t.Parallel()

	env := validEnvMap()
	env[config.EnvAuthURL] = "not-a-url"
	args := []string{"--role", "controller", "--nodeid", "node-uuid-123"}

	_, err := config.Parse(args, mockGetenv(env))
	if err == nil {
		t.Fatal("expected error for invalid OS_AUTH_URL, got nil")
	}
}

func TestSecretStringRedaction(t *testing.T) {
	t.Parallel()

	secret := config.SecretString("super-secret-key-12345")

	if secret.String() != "***" {
		t.Errorf("expected secret.String() to be %q, got %q", "***", secret.String())
	}
	if secret.Value() != "super-secret-key-12345" {
		t.Errorf("expected secret.Value() to be raw secret, got %q", secret.Value())
	}

	emptySecret := config.SecretString("")
	if emptySecret.String() != "" {
		t.Errorf("expected empty secret.String() to be empty, got %q", emptySecret.String())
	}

	formatted := fmt.Sprintf("%v", secret)
	if formatted != "***" {
		t.Errorf("expected %%v formatting to be %q, got %q", "***", formatted)
	}

	formattedStruct := fmt.Sprintf("%s", secret)
	if formattedStruct != "***" {
		t.Errorf("expected %%s formatting to be %q, got %q", "***", formattedStruct)
	}
}

func TestConfigStringRedaction(t *testing.T) {
	t.Parallel()

	env := validEnvMap()
	args := []string{"--role", "controller", "--nodeid", "node-uuid-123"}

	cfg, err := config.Parse(args, mockGetenv(env))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	str := cfg.String()
	if strings.Contains(str, env[config.EnvAccessKey]) {
		t.Errorf("Config.String() leaked AccessKey secret! Result: %s", str)
	}
	if strings.Contains(str, env[config.EnvSecretKey]) {
		t.Errorf("Config.String() leaked SecretKey secret! Result: %s", str)
	}
	if strings.Contains(str, env[config.EnvSecurityToken]) {
		t.Errorf("Config.String() leaked SecurityToken secret! Result: %s", str)
	}

	formatted := fmt.Sprintf("%v", cfg)
	if strings.Contains(formatted, env[config.EnvAccessKey]) {
		t.Errorf("fmt.Sprintf(%%v, cfg) leaked AccessKey secret! Result: %s", formatted)
	}
	if strings.Contains(formatted, env[config.EnvSecretKey]) {
		t.Errorf("fmt.Sprintf(%%v, cfg) leaked SecretKey secret! Result: %s", formatted)
	}
}

func TestErrorOutputDoesNotLeakSecrets(t *testing.T) {
	t.Parallel()

	secretAK := "SENSITIVE_ACCESS_KEY_VALUE_123"
	secretSK := "SENSITIVE_SECRET_KEY_VALUE_456"

	env := validEnvMap()
	env[config.EnvAccessKey] = secretAK
	env[config.EnvSecretKey] = secretSK
	// Intentionally trigger validation error by giving invalid AuthURL
	env[config.EnvAuthURL] = "http://invalid auth url"

	args := []string{"--role", "controller", "--nodeid", "node-uuid-123"}

	_, err := config.Parse(args, mockGetenv(env))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, secretAK) {
		t.Errorf("error output leaked AccessKey secret value! Error: %s", errMsg)
	}
	if strings.Contains(errMsg, secretSK) {
		t.Errorf("error output leaked SecretKey secret value! Error: %s", errMsg)
	}
}

// captureStderr swaps process stderr for a pipe and returns what is written after fn runs.
// Not safe for parallel use; callers must not mark their tests parallel.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stderr pipe: %v", err)
	}

	original := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = original }()

	fn()

	_ = w.Close()
	os.Stderr = original

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read captured stderr: %v", err)
	}
	return string(out)
}

// captureStdout swaps process stdout for a pipe and returns what is written after fn runs.
// Not safe for parallel use; callers must not mark their tests parallel.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}

	original := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = original }()

	fn()

	_ = w.Close()
	os.Stdout = original

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read captured stdout: %v", err)
	}
	return string(out)
}

func TestFlagParseFailureReportsOnlyThroughTheError(t *testing.T) {
	// Not parallel: captures process stderr.
	var parseErr error
	out := captureStderr(t, func() {
		_, parseErr = config.Parse(
			[]string{"--role", "controller", "--bogus-flag"},
			mockGetenv(validEnvMap()),
		)
	})

	if parseErr == nil {
		t.Fatal("expected a parse error for an unknown flag, got nil")
	}
	if errors.Is(parseErr, flag.ErrHelp) {
		t.Errorf("an unknown flag must not be reported as a help request: %v", parseErr)
	}
	if !strings.Contains(parseErr.Error(), "bogus-flag") {
		t.Errorf("expected the parse error to name the unknown flag, got: %v", parseErr)
	}
	if out != "" {
		t.Errorf("parse failure wrote to stderr: %q", out)
	}
}

func TestFlagHelpRequestPrintsUsage(t *testing.T) {
	// Not parallel: captures process stderr.
	var parseErr error
	out := captureStderr(t, func() {
		_, parseErr = config.Parse([]string{"--help"}, mockGetenv(validEnvMap()))
	})

	if !errors.Is(parseErr, flag.ErrHelp) {
		t.Fatalf("expected a help request to surface flag.ErrHelp, got: %v", parseErr)
	}
	if !strings.Contains(out, "Usage") {
		t.Errorf("expected a help request to print usage, got: %q", out)
	}
}

func TestVersionFlagPrintsBuildIdentityAndStops(t *testing.T) {
	// Not parallel: captures process stdout.
	var (
		cfg      *config.Config
		parseErr error
	)
	out := captureStdout(t, func() {
		cfg, parseErr = config.Parse([]string{"--version"}, mockGetenv(validEnvMap()))
	})

	if !errors.Is(parseErr, config.ErrVersionRequested) {
		t.Fatalf("expected a version request to surface ErrVersionRequested, got: %v", parseErr)
	}
	if cfg != nil {
		t.Errorf("expected no configuration from a version request, got: %v", cfg)
	}
	if !strings.Contains(out, "t-cloud-csi-driver") {
		t.Errorf("expected the version output to name the command, got: %q", out)
	}
	if !strings.Contains(out, version.Version()) {
		t.Errorf("expected the version output to report %q, got: %q", version.Version(), out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected exactly one line of version output, got: %q", out)
	}
}

// The version request is handled before the role check, so it needs no other input.
func TestVersionFlagNeedsNoRole(t *testing.T) {
	// Not parallel: captures process stdout.
	var parseErr error
	_ = captureStdout(t, func() {
		_, parseErr = config.Parse([]string{"--version"}, mockGetenv(map[string]string{}))
	})

	if !errors.Is(parseErr, config.ErrVersionRequested) {
		t.Fatalf("expected a version request without a role to be answered, got: %v", parseErr)
	}
}
