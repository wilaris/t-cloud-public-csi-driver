package driver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"wilaris.dev/t-cloud-public-csi-driver/internal/driver"
)

func TestNewIdentityService_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *config.Config
		wantErr bool
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: true,
		},
		{
			name: "empty driver name",
			cfg: &config.Config{
				DriverName: "",
				Version:    "v0.1.0",
			},
			wantErr: true,
		},
		{
			name: "empty version",
			cfg: &config.Config{
				DriverName: "evs.csi.t-cloud.wilaris.dev",
				Version:    "",
			},
			wantErr: true,
		},
		{
			name:    "valid config",
			cfg:     validTestConfig(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, err := driver.NewIdentityService(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewIdentityService() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && svc == nil {
				t.Fatal("expected non-nil IdentityService")
			}
		})
	}
}

func TestIdentityService_GRPC_GetPluginInfo(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	svc, err := driver.NewIdentityService(cfg)
	if err != nil {
		t.Fatalf("NewIdentityService failed: %v", err)
	}

	client := newIdentityClient(t, svc)

	res, err := client.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo failed: %v", err)
	}

	if res.GetName() != cfg.DriverName {
		t.Errorf("expected driver name %q, got %q", cfg.DriverName, res.GetName())
	}
	if res.GetVendorVersion() != cfg.Version {
		t.Errorf("expected version %q, got %q", cfg.Version, res.GetVendorVersion())
	}
}

func TestIdentityService_GRPC_GetPluginCapabilities(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	svc, err := driver.NewIdentityService(cfg)
	if err != nil {
		t.Fatalf("NewIdentityService failed: %v", err)
	}

	client := newIdentityClient(t, svc)

	res, err := client.GetPluginCapabilities(
		context.Background(),
		&csi.GetPluginCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("GetPluginCapabilities failed: %v", err)
	}

	caps := res.GetCapabilities()
	if len(caps) != 1 {
		t.Fatalf("expected 1 capability, got %d", len(caps))
	}

	serviceCap := caps[0].GetService()
	if serviceCap == nil {
		t.Fatal("expected service capability")
	}

	if serviceCap.GetType() != csi.PluginCapability_Service_CONTROLLER_SERVICE {
		t.Errorf(
			"expected CONTROLLER_SERVICE capability type, got %v",
			serviceCap.GetType(),
		)
	}
}

func TestIdentityService_GRPC_Probe(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	svc, err := driver.NewIdentityService(cfg)
	if err != nil {
		t.Fatalf("NewIdentityService failed: %v", err)
	}

	client := newIdentityClient(t, svc)

	res, err := client.Probe(context.Background(), &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}

	if res.GetReady() == nil || !res.GetReady().GetValue() {
		t.Errorf("expected Probe response Ready to be true, got %v", res.GetReady())
	}
}

func TestIdentityService_Direct_GetPluginInfo_InvalidState(t *testing.T) {
	t.Parallel()

	svc := &driver.IdentityService{}
	_, err := svc.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err == nil {
		t.Fatal("expected error for uninitialized IdentityService")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected status code InvalidArgument, got %v", st.Code())
	}
}

func TestIdentityService_GRPC_StatusMapping_Uninitialized(t *testing.T) {
	t.Parallel()

	client := newIdentityClient(t, &driver.IdentityService{})

	_, err := client.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err == nil {
		t.Fatal("expected gRPC error for uninitialized IdentityService over wire")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected status error, got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected status code InvalidArgument over gRPC transport, got %v", st.Code())
	}
}

func TestIdentityService_GRPC_CSISpecCompliance(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	svc, err := driver.NewIdentityService(cfg)
	if err != nil {
		t.Fatalf("NewIdentityService failed: %v", err)
	}

	client := newIdentityClient(t, svc)

	info, err := client.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo failed: %v", err)
	}

	name := info.GetName()
	if name == "" {
		t.Fatal("CSI spec requirement violation: plugin name must not be empty")
	}
	if len(name) > 63 {
		t.Errorf(
			"CSI spec requirement violation: plugin name %q exceeds maximum length of 63 characters (%d)",
			name,
			len(name),
		)
	}
	if !strings.Contains(name, ".") {
		t.Errorf(
			"CSI spec requirement violation: plugin name %q must follow reverse domain name notation",
			name,
		)
	}

	if info.GetVendorVersion() == "" {
		t.Fatal("CSI spec requirement violation: vendor_version must not be empty")
	}

	probeRes, err := client.Probe(context.Background(), &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if probeRes.GetReady() == nil || !probeRes.GetReady().GetValue() {
		t.Errorf("CSI spec requirement violation: Probe response ready field must be true")
	}
}
