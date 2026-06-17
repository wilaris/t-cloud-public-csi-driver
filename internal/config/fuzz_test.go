package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzResolveEndpoint(f *testing.F) {
	seeds := []string{
		"unix:///var/lib/kubelet/plugins/evs.csi.t-cloud.wilaris.dev/csi.sock",
		"unix:///tmp/csi.sock",
		"unix:///var/lib/kubelet/csi.sock",
		"unix://var/lib/kubelet/csi.sock",
		"unix:///var/lib/../csi.sock",
		"unix:///run/csi.sock?mode=0600",
		"unix:///run/csi.sock#main",
		"unix://user@/run/csi.sock",
		"tcp://127.0.0.1:9000",
		"tcp://0.0.0.0:9000",
		"tcp://[::1]:9000",
		"tcp://localhost:9000",
		"tcp://",
		"tcp://127.0.0.1:9000/csi",
		"tcp://user@127.0.0.1:9000",
		"tcp://127.0.0.1:9000?tls=on",
		"tcp://127.0.0.1:9000#main",
		"http://localhost/socket",
		"/var/lib/kubelet/csi.sock",
		"",
		"unix://",
		"unix:///",
		"unix:////",
		"   ",
		"unix:///tmp/socket\x00extra",
		"unix:///tmp/socket\r\n",
		"unix:///tmp/socket with spaces",
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, endpoint string) {
		network, address, err := resolveEndpoint(endpoint)
		if err != nil {
			if network != "" || address != "" {
				t.Fatalf(
					"resolveEndpoint returned non-empty results on error: net=%q, addr=%q",
					network,
					address,
				)
			}
			return
		}

		if network != "unix" && network != "tcp" {
			t.Fatalf("unexpected network returned on success: %q", network)
		}
		if address == "" {
			t.Fatal("expected non-empty address on success")
		}

		if network == "unix" {
			if !filepath.IsAbs(address) {
				t.Fatalf("unix address must be absolute, got %q", address)
			}
			if filepath.Clean(address) != address {
				t.Fatalf("unix address must be clean, got %q", address)
			}
			if strings.ContainsAny(address, "?#") {
				t.Fatalf("unix address must not contain query or fragment, got %q", address)
			}
		}

		if network == "tcp" {
			if strings.Contains(address, "/") {
				t.Fatalf("tcp address must carry no path, got %q", address)
			}
			if strings.ContainsAny(address, "?#") {
				t.Fatalf("tcp address must not contain query or fragment, got %q", address)
			}
		}
	})
}

func FuzzParseFlags(f *testing.F) {
	f.Add("controller", "unix:///tmp/csi.sock", "node-1", "custom-driver", "eu-de-01", "")
	f.Add("node", "unix:///tmp/csi.sock", "node-2", "evs.csi.t-cloud.wilaris.dev", "eu-de-02", "")
	f.Add("node", "tcp://127.0.0.1:9000", "", "evs.csi.t-cloud.wilaris.dev", "", "")
	f.Add("invalid-role", "", "", "", "", "--unknown")
	f.Add("", "unix:///tmp/csi.sock", "", "", "", "--version")
	f.Add("controller", "unix:///var/lib/../csi.sock", "", "", "", "--help")
	f.Add("controller", "http://invalid", "id", "driver", "zone", "")

	validEnv := map[string]string{
		EnvAuthURL:       "https://iam.eu-de.otc.t-systems.com/v3",
		EnvAccessKey:     "AKIAIOSFODNN7EXAMPLE",
		EnvSecretKey:     "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		EnvProjectID:     "1234567890abcdef1234567890abcdef",
		EnvRegionName:    "eu-de",
		EnvSecurityToken: "token-abc-123",
	}

	getenv := func(key string) string {
		return validEnv[key]
	}

	f.Fuzz(func(t *testing.T, role, endpoint, nodeID, driverName, az, extraArg string) {
		args := []string{}
		if role != "" {
			args = append(args, "--role", role)
		}
		if endpoint != "" {
			args = append(args, "--endpoint", endpoint)
		}
		if nodeID != "" {
			args = append(args, "--nodeid", nodeID)
		}
		if driverName != "" {
			args = append(args, "--driver-name", driverName)
		}
		if az != "" {
			args = append(args, "--availability-zone", az)
		}
		if extraArg != "" {
			args = append(args, extraArg)
		}

		cfg, err := Parse(args, getenv)
		if err != nil {
			if errors.Is(err, ErrVersionRequested) && cfg != nil {
				t.Fatalf("expected nil config when version requested, got: %v", cfg)
			}
			return
		}

		if cfg == nil {
			t.Fatal("expected non-nil config when err is nil")
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("parsed config failed Validate: %v", err)
		}

		if cfg.Role != RoleController && cfg.Role != RoleNode {
			t.Fatalf("unexpected role in parsed config: %q", cfg.Role)
		}

		if cfg.Endpoint == "" {
			t.Fatal("parsed config endpoint must not be empty")
		}
		if cfg.DriverName == "" {
			t.Fatal("parsed config driver-name must not be empty")
		}
		if cfg.Version == "" {
			t.Fatal("parsed config version must not be empty")
		}

		if cfg.Role == RoleController {
			if cfg.AuthURL == "" || cfg.AccessKey.Value() == "" || cfg.SecretKey.Value() == "" ||
				cfg.ProjectID == "" || cfg.RegionName == "" {
				t.Fatalf("controller config missing required cloud settings: %s", cfg)
			}
		}

		if cfg.Role == RoleNode {
			if cfg.AuthURL != "" || cfg.AccessKey.Value() != "" || cfg.SecretKey.Value() != "" ||
				cfg.ProjectID != "" || cfg.RegionName != "" || cfg.SecurityToken.Value() != "" {
				t.Fatalf("node config unexpectedly carries credentials: %s", cfg)
			}
		}

		cfgStr := cfg.String()
		if cfg.AccessKey.Value() != "" && strings.Contains(cfgStr, cfg.AccessKey.Value()) {
			t.Fatalf("Config.String() leaked AccessKey secret: %s", cfgStr)
		}
		if cfg.SecretKey.Value() != "" && strings.Contains(cfgStr, cfg.SecretKey.Value()) {
			t.Fatalf("Config.String() leaked SecretKey secret: %s", cfgStr)
		}
		if cfg.SecurityToken.Value() != "" && strings.Contains(cfgStr, cfg.SecurityToken.Value()) {
			t.Fatalf("Config.String() leaked SecurityToken secret: %s", cfgStr)
		}

		formatted := fmt.Sprintf("%v", cfg)
		if cfg.AccessKey.Value() != "" && strings.Contains(formatted, cfg.AccessKey.Value()) {
			t.Fatalf("fmt.Sprintf(%%v) leaked AccessKey secret: %s", formatted)
		}
		if cfg.SecretKey.Value() != "" && strings.Contains(formatted, cfg.SecretKey.Value()) {
			t.Fatalf("fmt.Sprintf(%%v) leaked SecretKey secret: %s", formatted)
		}
	})
}

func FuzzValidateCloudCredentials(f *testing.F) {
	f.Add(
		"https://iam.eu-de.otc.t-systems.com/v3",
		"AKIAIOSFODNN7EXAMPLE",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"1234567890abcdef1234567890abcdef",
		"eu-de",
		"token-abc-123",
	)
	f.Add("http://localhost:8080", "ak", "sk", "proj", "reg", "")
	f.Add("invalid-url", "ak", "sk", "proj", "reg", "")
	f.Add("", "", "", "", "", "")
	f.Add("http://", "ak", "sk", "proj", "reg", "")
	f.Add("https://iam.example.com", "", "sk", "proj", "reg", "")

	f.Fuzz(func(
		t *testing.T,
		authURL, accessKey, secretKey, projectID, regionName, securityToken string,
	) {
		cfg := &Config{
			Role:          RoleController,
			AuthURL:       authURL,
			AccessKey:     SecretString(accessKey),
			SecretKey:     SecretString(secretKey),
			ProjectID:     projectID,
			RegionName:    regionName,
			SecurityToken: SecretString(securityToken),
		}

		err := cfg.validateCloudCredentials()
		if err != nil {
			errMsg := err.Error()
			if len(accessKey) >= 4 && strings.Contains(errMsg, accessKey) {
				t.Fatalf("validateCloudCredentials leaked access key in error: %s", errMsg)
			}
			if len(secretKey) >= 4 && strings.Contains(errMsg, secretKey) {
				t.Fatalf("validateCloudCredentials leaked secret key in error: %s", errMsg)
			}
			if len(securityToken) >= 4 && strings.Contains(errMsg, securityToken) {
				t.Fatalf("validateCloudCredentials leaked security token in error: %s", errMsg)
			}
			return
		}

		if authURL == "" || accessKey == "" || secretKey == "" || projectID == "" ||
			regionName == "" {
			t.Fatalf("validateCloudCredentials succeeded with empty required field")
		}

		u, parseErr := url.Parse(authURL)
		if parseErr != nil || u.Scheme == "" || u.Host == "" {
			t.Fatalf("validateCloudCredentials succeeded with invalid AuthURL: %q", authURL)
		}
	})
}

func FuzzMetadataJSON(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"uuid":"9f0a1b2c-3d4e-5f60-7182-93a4b5c6d7e8","availability_zone":"eu-de-01"}`),
		[]byte(
			`{"uuid":"  9f0a1b2c-3d4e-5f60-7182-93a4b5c6d7e8  ","availability_zone":"\teu-de-01\n"}`,
		),
		[]byte(`{"uuid":"","availability_zone":""}`),
		[]byte(`{}`),
		[]byte(`{"uuid":123,"availability_zone":true}`),
		[]byte(`{"uuid":null,"availability_zone":null}`),
		[]byte(`{"uuid":"u","availability_zone":"z","extra":"field","nested":{"a":1}}`),
		[]byte(`[1, 2, 3]`),
		[]byte(`null`),
		[]byte(`{"uuid": not-json`),
		[]byte(`{"uuid":"` + strings.Repeat("x", 1000) + `"}`),
		[]byte(`{"\x00":"\x00"}`),
		[]byte(``),
		[]byte(`   `),
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		var doc struct {
			UUID             string `json:"uuid"`
			AvailabilityZone string `json:"availability_zone"`
		}

		decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(payload), metadataBodyLimit))
		if err := decoder.Decode(&doc); err != nil {
			return
		}

		facts := nodeFacts{
			serverUUID:       strings.TrimSpace(doc.UUID),
			availabilityZone: strings.TrimSpace(doc.AvailabilityZone),
		}

		if strings.TrimSpace(facts.serverUUID) != facts.serverUUID {
			t.Fatalf("serverUUID was not trimmed: %q", facts.serverUUID)
		}
		if strings.TrimSpace(facts.availabilityZone) != facts.availabilityZone {
			t.Fatalf("availabilityZone was not trimmed: %q", facts.availabilityZone)
		}
	})
}

func FuzzParseRole(f *testing.F) {
	seeds := []string{
		"controller",
		"node",
		"",
		" ",
		"controller ",
		" node\t",
		"CONTROLLER",
		"Node",
		"other",
		"controller\x00",
		"node\n",
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, rawRole string) {
		role, err := parseRole(strings.TrimSpace(rawRole))
		trimmed := strings.TrimSpace(rawRole)
		if trimmed == "controller" {
			if err != nil {
				t.Fatalf("expected clean parse for trimmed role %q, got: %v", trimmed, err)
			}
			if role != RoleController {
				t.Fatalf("expected RoleController, got %q", role)
			}
			return
		}
		if trimmed == "node" {
			if err != nil {
				t.Fatalf("expected clean parse for trimmed role %q, got: %v", trimmed, err)
			}
			if role != RoleNode {
				t.Fatalf("expected RoleNode, got %q", role)
			}
			return
		}
		if err == nil {
			t.Fatalf("expected error for invalid role %q, got: %q", trimmed, role)
		}
	})
}
