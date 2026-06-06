package evs_test

import (
	"strings"
	"testing"

	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

func TestLoadConfig_ValidEnv(t *testing.T) {
	env := map[string]string{
		evs.EnvAuthURL:       "https://iam.eu-de.otc.t-systems.com/v3",
		evs.EnvAccessKey:     "test-access-key",
		evs.EnvSecretKey:     "test-secret-key",
		evs.EnvProjectID:     "1234567890abcdef1234567890abcdef",
		evs.EnvRegionName:    "eu-de",
		evs.EnvSecurityToken: "test-security-token",
	}

	cfg, err := evs.LoadConfig(func(key string) string {
		return env[key]
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if cfg.AuthURL != env[evs.EnvAuthURL] {
		t.Errorf("AuthURL = %q, want %q", cfg.AuthURL, env[evs.EnvAuthURL])
	}
	if cfg.AccessKey != env[evs.EnvAccessKey] {
		t.Errorf("AccessKey = %q, want %q", cfg.AccessKey, env[evs.EnvAccessKey])
	}
	if cfg.SecretKey != env[evs.EnvSecretKey] {
		t.Errorf("SecretKey = %q, want %q", cfg.SecretKey, env[evs.EnvSecretKey])
	}
	if cfg.ProjectID != env[evs.EnvProjectID] {
		t.Errorf("ProjectID = %q, want %q", cfg.ProjectID, env[evs.EnvProjectID])
	}
	if cfg.RegionName != env[evs.EnvRegionName] {
		t.Errorf("RegionName = %q, want %q", cfg.RegionName, env[evs.EnvRegionName])
	}
	if cfg.SecurityToken != env[evs.EnvSecurityToken] {
		t.Errorf("SecurityToken = %q, want %q", cfg.SecurityToken, env[evs.EnvSecurityToken])
	}
}

func TestLoadConfig_MissingRequiredEnv(t *testing.T) {
	required := []string{
		evs.EnvAuthURL,
		evs.EnvAccessKey,
		evs.EnvSecretKey,
		evs.EnvProjectID,
		evs.EnvRegionName,
	}

	baseEnv := map[string]string{
		evs.EnvAuthURL:    "https://iam.eu-de.otc.t-systems.com/v3",
		evs.EnvAccessKey:  "test-access-key",
		evs.EnvSecretKey:  "test-secret-key",
		evs.EnvProjectID:  "1234567890abcdef1234567890abcdef",
		evs.EnvRegionName: "eu-de",
	}

	for _, missingKey := range required {
		t.Run("missing_"+missingKey, func(t *testing.T) {
			cfg, err := evs.LoadConfig(func(key string) string {
				if key == missingKey {
					return ""
				}
				return baseEnv[key]
			})
			if err == nil {
				t.Fatalf("expected error for missing %s, got nil (cfg: %+v)", missingKey, cfg)
			}
			if !strings.Contains(err.Error(), missingKey) {
				t.Errorf("expected error message to mention %s, got %q", missingKey, err.Error())
			}
		})
	}
}

func TestLoadConfig_UnacceptedAuthMethodsAndAliasesNotSupported(t *testing.T) {
	unacceptedEnv := map[string]string{
		"OS_USERNAME":    "test-user",
		"OS_PASSWORD":    "test-pass",
		"OS_TENANT_NAME": "test-tenant",
		"OS_DOMAIN_NAME": "test-domain",
	}

	cfg, err := evs.LoadConfig(func(key string) string {
		return unacceptedEnv[key]
	})
	if err == nil {
		t.Fatalf("expected LoadConfig to fail when required AK/SK vars are missing, got: %+v", cfg)
	}
}

func TestNewProviderClient_MissingRequiredConfig(t *testing.T) {
	cfg := evs.Config{
		AuthURL:    "https://iam.eu-de.otc.t-systems.com/v3",
		AccessKey:  "MY_ACCESS_KEY_123",
		ProjectID:  "proj-789",
		RegionName: "eu-de",
	}

	client, err := evs.NewProviderClient(cfg)
	if err == nil {
		t.Fatalf(
			"expected NewProviderClient to fail with missing SecretKey, got client: %v",
			client,
		)
	}
}

func TestNewProviderClient_SecretContainment(t *testing.T) {
	secretAK := "SECRET_AK_VAL_999"
	secretSK := "SECRET_SK_VAL_888"
	secretToken := "SECRET_TOKEN_VAL_777"

	cfg := evs.Config{
		AuthURL:       ":invalid-url-scheme-that-causes-parse-error",
		AccessKey:     secretAK,
		SecretKey:     secretSK,
		ProjectID:     "proj-789",
		RegionName:    "eu-de",
		SecurityToken: secretToken,
	}

	_, err := evs.NewProviderClient(cfg)
	if err == nil {
		t.Fatal("expected error for invalid AuthURL, got nil")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, secretAK) {
		t.Errorf(
			"secret containment violation: error message contains AccessKey %q: %s",
			secretAK,
			errMsg,
		)
	}
	if strings.Contains(errMsg, secretSK) {
		t.Errorf(
			"secret containment violation: error message contains SecretKey %q: %s",
			secretSK,
			errMsg,
		)
	}
	if strings.Contains(errMsg, secretToken) {
		t.Errorf(
			"secret containment violation: error message contains SecurityToken %q: %s",
			secretToken,
			errMsg,
		)
	}
}
