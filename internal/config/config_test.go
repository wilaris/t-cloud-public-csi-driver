package config_test

import (
	"fmt"
	"strings"
	"testing"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
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
		"--nodeid", "68e1a123-4567-89ab-cdef-0123456789ab",
		"--endpoint", "unix:///tmp/csi.sock",
		"--driver-name", "my-custom-driver",
		"--version", "v1.2.3",
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
	if cfg.Version != "v1.2.3" {
		t.Errorf("expected Version %q, got %q", "v1.2.3", cfg.Version)
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
	args := []string{"--nodeid", "node-uuid-123"}

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
	if cfg.Version != config.DefaultVersion {
		t.Errorf("expected default Version %q, got %q", config.DefaultVersion, cfg.Version)
	}
}

func TestMissingNodeID(t *testing.T) {
	t.Parallel()

	env := validEnvMap()
	args := []string{} // missing --nodeid

	_, err := config.Parse(args, mockGetenv(env))
	if err == nil {
		t.Fatal("expected error for missing --nodeid, got nil")
	}
	if !strings.Contains(err.Error(), "--nodeid") {
		t.Errorf("expected error message to mention --nodeid, got: %v", err)
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
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := validEnvMap()
			args := []string{"--nodeid", "node-uuid-123", "--endpoint", tc.endpoint}

			_, err := config.Parse(args, mockGetenv(env))
			if err == nil {
				t.Fatalf("expected error for endpoint %q, got nil", tc.endpoint)
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
		reqVar := reqVar
		t.Run("missing_"+reqVar, func(t *testing.T) {
			t.Parallel()
			env := validEnvMap()
			delete(env, reqVar)
			args := []string{"--nodeid", "node-uuid-123"}

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
	args := []string{"--nodeid", "node-uuid-123"}

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
	args := []string{"--nodeid", "node-uuid-123"}

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

	args := []string{"--nodeid", "node-uuid-123"}

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
