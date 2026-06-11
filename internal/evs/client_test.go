package evs_test

import (
	"strings"
	"testing"

	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

func TestNewProviderClient_MissingRequiredConfig(t *testing.T) {
	cfg := evs.Config{
		AuthURL:    "https://iam.eu-de.otc.t-systems.com/v3",
		AccessKey:  "MY_ACCESS_KEY_123",
		ProjectID:  "proj-789",
		RegionName: "eu-de",
	}

	client, err := evs.NewProviderClient(t.Context(), cfg)
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

	_, err := evs.NewProviderClient(t.Context(), cfg)
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
