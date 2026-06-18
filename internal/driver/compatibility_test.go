package driver_test

import (
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/driver"
)

// TestCompatibility_PluginCapabilities verifies that the plugin advertises only the exact
// accepted plugin capabilities and no unadvertised extensions.
func TestCompatibility_PluginCapabilities(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	svc, err := driver.NewIdentityService(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewIdentityService failed: %v", err)
	}

	client := newIdentityClient(t, svc)
	res, err := client.GetPluginCapabilities(t.Context(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities failed: %v", err)
	}

	caps := res.GetCapabilities()
	if len(caps) != 2 {
		t.Fatalf("expected exactly 2 plugin capabilities, got %d", len(caps))
	}

	var serviceTypes []csi.PluginCapability_Service_Type
	for _, c := range caps {
		svcCap := c.GetService()
		if svcCap == nil {
			t.Error("expected service capability in plugin capability response")
			continue
		}
		serviceTypes = append(serviceTypes, svcCap.GetType())
	}

	slices.Sort(serviceTypes)
	wantTypes := []csi.PluginCapability_Service_Type{
		csi.PluginCapability_Service_CONTROLLER_SERVICE,
		csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
	}
	slices.Sort(wantTypes)

	if !slices.Equal(serviceTypes, wantTypes) {
		t.Errorf("plugin capabilities = %v, want %v", serviceTypes, wantTypes)
	}
}

// TestCompatibility_ControllerCapabilities verifies that Controller advertises only volume
// provisioning and attachment capabilities, explicitly excluding snapshots and expansion.
func TestCompatibility_ControllerCapabilities(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	svc, err := driver.NewControllerService(newMockEVSClient(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	client := newControllerClient(t, svc)
	res, err := client.ControllerGetCapabilities(
		t.Context(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("ControllerGetCapabilities failed: %v", err)
	}

	caps := res.GetCapabilities()
	if len(caps) != 2 {
		t.Fatalf("expected exactly 2 controller capabilities, got %d", len(caps))
	}

	var rpcTypes []csi.ControllerServiceCapability_RPC_Type
	for _, c := range caps {
		rpc := c.GetRpc()
		if rpc == nil {
			t.Error("expected RPC capability in controller capability response")
			continue
		}
		rpcTypes = append(rpcTypes, rpc.GetType())
	}

	slices.Sort(rpcTypes)
	wantRPCs := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	}
	slices.Sort(wantRPCs)

	if !slices.Equal(rpcTypes, wantRPCs) {
		t.Errorf("controller capabilities = %v, want %v", rpcTypes, wantRPCs)
	}

	// Verify deferred capabilities are not advertised.
	prohibited := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_GET_VOLUME_HEALTH,
		csi.ControllerServiceCapability_RPC_LIST_VOLUME_HEALTH,
		csi.ControllerServiceCapability_RPC_PUBLISH_READONLY,
	}
	for _, p := range prohibited {
		if slices.Contains(rpcTypes, p) {
			t.Errorf("prohibited controller capability %v was advertised", p)
		}
	}
}

// TestCompatibility_NodeCapabilities verifies that Node advertises only stage/unstage,
// explicitly excluding expansion and volume stats.
func TestCompatibility_NodeCapabilities(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	svc, err := driver.NewNodeService(&recordingMounter{}, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)
	res, err := client.NodeGetCapabilities(t.Context(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities failed: %v", err)
	}

	caps := res.GetCapabilities()
	if len(caps) != 1 {
		t.Fatalf("expected exactly 1 node capability, got %d", len(caps))
	}

	rpc := caps[0].GetRpc()
	if rpc == nil {
		t.Fatal("expected RPC capability in node capability response")
	}

	if rpc.GetType() != csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME {
		t.Errorf(
			"node capability = %v, want %v",
			rpc.GetType(),
			csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		)
	}

	// Verify deferred capabilities are not advertised.
	prohibited := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_GET_VOLUME_HEALTH,
		csi.NodeServiceCapability_RPC_GET_STORAGE_HEALTH,
	}
	for _, p := range prohibited {
		if rpc.GetType() == p {
			t.Errorf("prohibited node capability %v was advertised", p)
		}
	}
}

// TestCompatibility_DriverIdentity verifies the fixed driver name and non-empty vendor version.
func TestCompatibility_DriverIdentity(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	svc, err := driver.NewIdentityService(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewIdentityService failed: %v", err)
	}

	client := newIdentityClient(t, svc)
	res, err := client.GetPluginInfo(t.Context(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo failed: %v", err)
	}

	const wantName = "evs.csi.t-cloud.wilaris.dev"
	if res.GetName() != wantName {
		t.Errorf("plugin name = %q, want %q", res.GetName(), wantName)
	}
	if res.GetVendorVersion() == "" {
		t.Error("vendor version must not be empty")
	}
}
