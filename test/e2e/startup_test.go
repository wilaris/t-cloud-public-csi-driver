//go:build e2e

package e2e

import (
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
)

// TestRoleStartup covers startup facts offline tests cannot reach: node identity from the real
// metadata service. A controller starts only after real authentication succeeds.
func TestRoleStartup(t *testing.T) {
	h := runState
	ctx := h.context()

	clause(t, catalogue.CheckNodeIdentityFromMetadata, func(t *testing.T) {
		// The node process was started with neither override, so both
		// values can only have come from the metadata document.
		info, err := h.node.nodeClient().NodeGetInfo(ctx, &csi.NodeGetInfoRequest{})
		if err != nil {
			t.Fatalf("read node info: %v", err)
		}

		if info.GetNodeId() != h.identity.ServerID {
			t.Fatalf(
				"the node reports %q, not the server the metadata document named",
				info.GetNodeId(),
			)
		}

		zone := info.GetAccessibleTopology().GetSegments()[driver.TopologyZoneKey]
		if zone != h.identity.Zone {
			t.Fatalf("the node reports zone %q, not the one the metadata document named", zone)
		}
		h.evidence.Observe("node_reported_id", info.GetNodeId())
		h.evidence.Observe("node_reported_zone", zone)
	})

	clause(t, catalogue.CheckControllerServesIdentity, func(t *testing.T) {
		info, err := h.controller.identityClient().
			GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
		if err != nil {
			t.Fatalf("read plugin info: %v", err)
		}
		if info.GetName() != config.DefaultDriverName {
			t.Fatalf("the controller reports plugin name %q", info.GetName())
		}
		h.evidence.Observe("plugin_name", info.GetName())
		h.evidence.Observe("plugin_version", info.GetVendorVersion())
	})

	clause(t, catalogue.CheckControllerServesController, func(t *testing.T) {
		caps, err := h.controller.controllerClient().ControllerGetCapabilities(
			ctx,
			&csi.ControllerGetCapabilitiesRequest{},
		)
		if err != nil {
			t.Fatalf("read controller capabilities: %v", err)
		}
		if len(caps.GetCapabilities()) == 0 {
			t.Fatalf("the controller advertises no capability")
		}
	})

	clause(t, catalogue.CheckControllerServesNoNode, func(t *testing.T) {
		_, err := h.controller.nodeClient().NodeGetInfo(ctx, &csi.NodeGetInfoRequest{})
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("a node call on the controller answered %v", err)
		}
	})
}
