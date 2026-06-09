package driver_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

type mockEVSClient struct {
	volumes     map[string]*evs.Volume
	attachments map[string]string
	lastCreate  evs.CreateVolumeOpts
}

func newMockEVSClient() *mockEVSClient {
	return &mockEVSClient{
		volumes:     make(map[string]*evs.Volume),
		attachments: make(map[string]string),
	}
}

func (m *mockEVSClient) CreateVolume(
	ctx context.Context,
	opts evs.CreateVolumeOpts,
) (*evs.Volume, error) {
	m.lastCreate = opts
	vol := &evs.Volume{
		ID:               fmt.Sprintf("vol-%s", opts.Name),
		Name:             opts.Name,
		Status:           "available",
		Size:             opts.Size,
		AvailabilityZone: opts.AvailabilityZone,
		VolumeType:       opts.VolumeType,
		Tags:             map[string]string{evs.OwnershipTagKey: evs.OwnershipTagValue},
	}
	m.volumes[vol.ID] = vol
	return vol, nil
}

func (m *mockEVSClient) GetVolume(ctx context.Context, id string) (*evs.Volume, error) {
	vol, ok := m.volumes[id]
	if !ok {
		return nil, fmt.Errorf("volume %s: %w", id, evs.ErrNotFound)
	}
	return vol, nil
}

func (m *mockEVSClient) DiscoverVolume(
	ctx context.Context,
	opts evs.DiscoverVolumeOpts,
) (*evs.Volume, error) {
	nameExists := false
	for _, vol := range m.volumes {
		if vol.Name != opts.Name {
			continue
		}
		nameExists = true
		if vol.Tags[evs.OwnershipTagKey] != evs.OwnershipTagValue {
			continue
		}
		if vol.Status != "available" && vol.Status != "in-use" {
			continue
		}
		if vol.AvailabilityZone != opts.AvailabilityZone || vol.VolumeType != opts.VolumeType {
			continue
		}
		if vol.Size < opts.MinSizeGiB {
			continue
		}
		if opts.MaxSizeGiB > 0 && vol.Size > opts.MaxSizeGiB {
			continue
		}
		return vol, nil
	}

	if nameExists {
		return nil, fmt.Errorf("volume %s: %w", opts.Name, evs.ErrConflict)
	}
	return nil, fmt.Errorf("volume %s: %w", opts.Name, evs.ErrNotFound)
}

func (m *mockEVSClient) DeleteVolume(ctx context.Context, id string) error {
	vol, ok := m.volumes[id]
	if !ok {
		return fmt.Errorf("volume %s: %w", id, evs.ErrNotFound)
	}
	if vol.Tags[evs.OwnershipTagKey] != evs.OwnershipTagValue {
		return fmt.Errorf("volume %s: %w", id, evs.ErrNotOwned)
	}
	delete(m.volumes, id)
	delete(m.attachments, id)
	return nil
}

func (m *mockEVSClient) AttachVolume(ctx context.Context, volumeID, serverID string) error {
	if volumeID == "" || serverID == "" {
		return fmt.Errorf("attach volume: %w", evs.ErrInvalidArgument)
	}
	vol, ok := m.volumes[volumeID]
	if !ok {
		return fmt.Errorf("volume %s: %w", volumeID, evs.ErrNotFound)
	}
	if currentServer, attached := m.attachments[volumeID]; attached {
		if currentServer == serverID {
			return nil
		}
		return fmt.Errorf("volume %s attached to %s: %w", volumeID, currentServer, evs.ErrConflict)
	}
	m.attachments[volumeID] = serverID
	vol.Status = "in-use"
	return nil
}

func (m *mockEVSClient) DetachVolume(ctx context.Context, volumeID, serverID string) error {
	if volumeID == "" || serverID == "" {
		return fmt.Errorf("detach volume: %w", evs.ErrInvalidArgument)
	}
	if _, ok := m.volumes[volumeID]; !ok {
		return fmt.Errorf("volume %s: %w", volumeID, evs.ErrNotFound)
	}
	currentServer, attached := m.attachments[volumeID]
	if !attached || currentServer != serverID {
		return nil
	}
	delete(m.attachments, volumeID)
	if vol, ok := m.volumes[volumeID]; ok {
		vol.Status = "available"
	}
	return nil
}

func TestNewControllerService(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	cfg := validTestConfig()

	svc, err := driver.NewControllerService(client, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}

	_, err = driver.NewControllerService(nil, cfg)
	if err == nil {
		t.Error("expected error for nil client")
	}

	_, err = driver.NewControllerService(client, nil)
	if err == nil {
		t.Error("expected error for nil config")
	}
}

func TestControllerGetCapabilities(t *testing.T) {
	t.Parallel()

	svc, _ := driver.NewControllerService(newMockEVSClient(), validTestConfig())
	resp, err := svc.ControllerGetCapabilities(
		context.Background(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Capabilities) != 2 {
		t.Fatalf("expected 2 capabilities, got %d", len(resp.Capabilities))
	}
	cap0 := resp.Capabilities[0].GetRpc().GetType()
	if cap0 != csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME {
		t.Errorf("expected CREATE_DELETE_VOLUME capability, got %v", cap0)
	}
	cap1 := resp.Capabilities[1].GetRpc().GetType()
	if cap1 != csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME {
		t.Errorf("expected PUBLISH_UNPUBLISH_VOLUME capability, got %v", cap1)
	}
}

func TestControllerPublishVolume_HappyPath(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc, _ := driver.NewControllerService(client, validTestConfig())

	vol, _ := client.CreateVolume(context.Background(), evs.CreateVolumeOpts{
		Name:             "pub-vol",
		Size:             1,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         vol.ID,
		NodeId:           "server-uuid-1234",
		VolumeCapability: accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}

	resp, err := svc.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify volume status in mock
	updatedVol, _ := client.GetVolume(context.Background(), vol.ID)
	if updatedVol.Status != "in-use" {
		t.Errorf("expected volume status in-use, got %s", updatedVol.Status)
	}
}

func TestControllerPublishVolume_Idempotency(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc, _ := driver.NewControllerService(client, validTestConfig())

	vol, _ := client.CreateVolume(context.Background(), evs.CreateVolumeOpts{
		Name:             "idempotent-pub-vol",
		Size:             1,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         vol.ID,
		NodeId:           "server-uuid-1234",
		VolumeCapability: accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}

	// First publish
	_, err := svc.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("first publish failed: %v", err)
	}

	// Second publish to same node -> idempotent success
	_, err = svc.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("second publish failed: %v", err)
	}

	// Publish to different node -> conflict error
	diffNodeReq := &csi.ControllerPublishVolumeRequest{
		VolumeId:         vol.ID,
		NodeId:           "server-uuid-5678",
		VolumeCapability: req.VolumeCapability,
	}
	_, err = svc.ControllerPublishVolume(context.Background(), diffNodeReq)
	if status.Code(err) != codes.AlreadyExists && status.Code(err) != codes.FailedPrecondition {
		t.Errorf(
			"expected conflict/already exists error for publishing to different node, got %v",
			err,
		)
	}
}

func TestControllerPublishVolume_ValidationFailures(t *testing.T) {
	t.Parallel()

	svc, _ := driver.NewControllerService(newMockEVSClient(), validTestConfig())

	// Nil request
	_, err := svc.ControllerPublishVolume(context.Background(), nil)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for nil request, got %v", err)
	}

	// Empty volume ID
	_, err = svc.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		NodeId:           "node-1",
		VolumeCapability: accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty volume_id, got %v", err)
	}

	// Empty node ID
	_, err = svc.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         "vol-1",
		VolumeCapability: accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty node_id, got %v", err)
	}

	// Nil capability
	_, err = svc.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: "vol-1",
		NodeId:   "node-1",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for nil capability, got %v", err)
	}

	// Unsupported capability mode (multi-node)
	_, err = svc.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: "vol-1",
		NodeId:   "node-1",
		VolumeCapability: accessModeCapability(
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for multi-node mode, got %v", err)
	}
}

func TestControllerUnpublishVolume_HappyPath(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc, _ := driver.NewControllerService(client, validTestConfig())

	vol, _ := client.CreateVolume(context.Background(), evs.CreateVolumeOpts{
		Name:             "unpub-vol",
		Size:             1,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})

	_ = client.AttachVolume(context.Background(), vol.ID, "server-uuid-1234")

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: vol.ID,
		NodeId:   "server-uuid-1234",
	}

	resp, err := svc.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Idempotent unpublish
	_, err = svc.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error on second unpublish: %v", err)
	}
}

func TestControllerUnpublishVolume_ValidationFailures(t *testing.T) {
	t.Parallel()

	svc, _ := driver.NewControllerService(newMockEVSClient(), validTestConfig())

	// Nil request
	_, err := svc.ControllerUnpublishVolume(context.Background(), nil)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for nil request, got %v", err)
	}

	// Empty volume ID
	_, err = svc.ControllerUnpublishVolume(
		context.Background(),
		&csi.ControllerUnpublishVolumeRequest{
			NodeId: "node-1",
		},
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty volume_id, got %v", err)
	}

	// Empty node ID
	_, err = svc.ControllerUnpublishVolume(
		context.Background(),
		&csi.ControllerUnpublishVolumeRequest{
			VolumeId: "vol-1",
		},
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty node_id, got %v", err)
	}
}

func TestCreateVolume_HappyPath(t *testing.T) {
	t.Parallel()

	svc, _ := driver.NewControllerService(newMockEVSClient(), validTestConfig())
	req := &csi.CreateVolumeRequest{
		Name: "test-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			mountCapability(""),
		},
		Parameters: map[string]string{
			"type": "SSD",
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{
					Segments: map[string]string{
						driver.TopologyZoneKey: "eu-de-01",
					},
				},
			},
		},
	}

	resp, err := svc.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Volume.VolumeId == "" {
		t.Error("expected non-empty VolumeId")
	}
	if resp.Volume.CapacityBytes != 10*1024*1024*1024 {
		t.Errorf("expected capacity 10GiB, got %d", resp.Volume.CapacityBytes)
	}
	if len(resp.Volume.AccessibleTopology) != 1 ||
		resp.Volume.AccessibleTopology[0].Segments[driver.TopologyZoneKey] != "eu-de-01" {
		t.Errorf("unexpected accessible topology: %+v", resp.Volume.AccessibleTopology)
	}
}

func TestCreateVolume_TopologyRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	identityService, err := driver.NewIdentityService(cfg)
	if err != nil {
		t.Fatalf("NewIdentityService failed: %v", err)
	}
	nodeService, err := driver.NewNodeService(&fakeMounter{}, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}
	evsClient := newMockEVSClient()
	controllerService, err := driver.NewControllerService(evsClient, cfg)
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	conn := serveCSI(t, func(server *grpc.Server) {
		csi.RegisterIdentityServer(server, identityService)
		csi.RegisterNodeServer(server, nodeService)
		csi.RegisterControllerServer(server, controllerService)
	})

	identityClient := csi.NewIdentityClient(conn)
	capabilities, err := identityClient.GetPluginCapabilities(
		context.Background(),
		&csi.GetPluginCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("GetPluginCapabilities failed: %v", err)
	}
	topologySupported := false
	for _, capability := range capabilities.GetCapabilities() {
		if capability.GetService().GetType() ==
			csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS {
			topologySupported = true
			break
		}
	}
	if !topologySupported {
		t.Fatal("VOLUME_ACCESSIBILITY_CONSTRAINTS is not advertised")
	}

	nodeClient := csi.NewNodeClient(conn)
	nodeInfo, err := nodeClient.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo failed: %v", err)
	}
	nodeTopology := nodeInfo.GetAccessibleTopology()
	if nodeTopology == nil {
		t.Fatal("NodeGetInfo did not return accessible topology")
	}
	nodeZone, ok := nodeTopology.GetSegments()[driver.TopologyZoneKey]
	if !ok {
		t.Fatalf("NodeGetInfo topology does not contain %q", driver.TopologyZoneKey)
	}
	if nodeZone != cfg.AvailabilityZone {
		t.Fatalf("expected node zone %q, got %q", cfg.AvailabilityZone, nodeZone)
	}

	controllerClient := csi.NewControllerClient(conn)
	request := &csi.CreateVolumeRequest{
		Name: "topology-round-trip",
		VolumeCapabilities: []*csi.VolumeCapability{
			mountCapability(""),
		},
		Parameters: map[string]string{
			"type": "SSD",
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{nodeTopology},
		},
	}
	requestZone := request.GetAccessibilityRequirements().GetRequisite()[0].GetSegments()[driver.TopologyZoneKey]
	if requestZone != nodeZone {
		t.Fatalf("expected Controller request zone %q, got %q", nodeZone, requestZone)
	}

	created, err := controllerClient.CreateVolume(context.Background(), request)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if evsClient.lastCreate.AvailabilityZone != requestZone {
		t.Fatalf(
			"expected EVS create zone %q, got %q",
			requestZone,
			evsClient.lastCreate.AvailabilityZone,
		)
	}

	accessibleTopology := created.GetVolume().GetAccessibleTopology()
	if len(accessibleTopology) != 1 {
		t.Fatalf("expected one accessible topology, got %d", len(accessibleTopology))
	}
	createdZone, ok := accessibleTopology[0].GetSegments()[driver.TopologyZoneKey]
	if !ok {
		t.Fatalf("created volume topology does not contain %q", driver.TopologyZoneKey)
	}
	if createdZone != evsClient.lastCreate.AvailabilityZone {
		t.Errorf(
			"expected created volume zone %q, got %q",
			evsClient.lastCreate.AvailabilityZone,
			createdZone,
		)
	}
}

func TestCreateVolume_Idempotency(t *testing.T) {
	t.Parallel()

	svc, _ := driver.NewControllerService(newMockEVSClient(), validTestConfig())
	req := &csi.CreateVolumeRequest{
		Name: "idempotent-vol",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 5 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		},
		Parameters: map[string]string{
			"type": "SSD",
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{driver.TopologyZoneKey: "eu-de-01"}},
			},
		},
	}

	resp1, err := svc.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("first CreateVolume failed: %v", err)
	}

	resp2, err := svc.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateVolume failed: %v", err)
	}

	if resp1.Volume.VolumeId != resp2.Volume.VolumeId {
		t.Errorf(
			"expected identical volume ID, got %s and %s",
			resp1.Volume.VolumeId,
			resp2.Volume.VolumeId,
		)
	}

	compatibleReq := &csi.CreateVolumeRequest{
		Name: "idempotent-vol",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * 1024 * 1024 * 1024,
			LimitBytes:    10 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities:        req.VolumeCapabilities,
		Parameters:                req.Parameters,
		AccessibilityRequirements: req.AccessibilityRequirements,
	}

	resp3, err := svc.CreateVolume(context.Background(), compatibleReq)
	if err != nil {
		t.Fatalf("compatible CreateVolume retry failed: %v", err)
	}
	if resp3.Volume.VolumeId != resp1.Volume.VolumeId {
		t.Errorf(
			"expected compatible retry to return volume ID %s, got %s",
			resp1.Volume.VolumeId,
			resp3.Volume.VolumeId,
		)
	}
}

func TestCreateVolume_IdempotencyRejectsIncompatibleVolume(t *testing.T) {
	t.Parallel()

	const gib = int64(1024 * 1024 * 1024)

	tests := []struct {
		name          string
		sizeGiB       int
		volumeType    string
		zone          string
		requiredBytes int64
		limitBytes    int64
	}{
		{
			name:          "below required size",
			sizeGiB:       5,
			volumeType:    "SSD",
			zone:          "eu-de-01",
			requiredBytes: 10 * gib,
			limitBytes:    20 * gib,
		},
		{
			name:          "above size limit",
			sizeGiB:       20,
			volumeType:    "SSD",
			zone:          "eu-de-01",
			requiredBytes: 5 * gib,
			limitBytes:    10 * gib,
		},
		{
			name:          "different volume type",
			sizeGiB:       10,
			volumeType:    "SAS",
			zone:          "eu-de-01",
			requiredBytes: 5 * gib,
			limitBytes:    20 * gib,
		},
		{
			name:          "different availability zone",
			sizeGiB:       10,
			volumeType:    "SSD",
			zone:          "eu-de-02",
			requiredBytes: 5 * gib,
			limitBytes:    20 * gib,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newMockEVSClient()
			_, err := client.CreateVolume(context.Background(), evs.CreateVolumeOpts{
				Name:             "existing-volume",
				Size:             tt.sizeGiB,
				AvailabilityZone: tt.zone,
				VolumeType:       tt.volumeType,
			})
			if err != nil {
				t.Fatalf("create existing volume: %v", err)
			}

			svc, err := driver.NewControllerService(client, validTestConfig())
			if err != nil {
				t.Fatalf("NewControllerService failed: %v", err)
			}

			_, err = svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name: "existing-volume",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: tt.requiredBytes,
					LimitBytes:    tt.limitBytes,
				},
				VolumeCapabilities: []*csi.VolumeCapability{
					mountCapability(""),
				},
				Parameters: map[string]string{
					"type": "SSD",
				},
				AccessibilityRequirements: &csi.TopologyRequirement{
					Requisite: []*csi.Topology{
						{Segments: map[string]string{driver.TopologyZoneKey: "eu-de-01"}},
					},
				},
			})
			if status.Code(err) != codes.AlreadyExists {
				t.Errorf("expected AlreadyExists for incompatible volume, got %v", err)
			}
		})
	}
}

func TestCreateVolume_UnsupportedAccessMode(t *testing.T) {
	t.Parallel()

	svc, _ := driver.NewControllerService(newMockEVSClient(), validTestConfig())
	req := &csi.CreateVolumeRequest{
		Name: "multi-node-vol",
		VolumeCapabilities: []*csi.VolumeCapability{
			accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{driver.TopologyZoneKey: "eu-de-01"}},
			},
		},
	}

	_, err := svc.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for multi-node access mode, got %v", err)
	}
}

func TestCreateVolume_ValidationFailures(t *testing.T) {
	t.Parallel()

	svc, _ := driver.NewControllerService(newMockEVSClient(), validTestConfig())

	// Nil request
	_, err := svc.CreateVolume(context.Background(), nil)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for nil request, got %v", err)
	}

	// Empty name
	_, err = svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty name, got %v", err)
	}

	// Empty capabilities
	_, err = svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "vol-1"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty capabilities, got %v", err)
	}

	// Missing availability zone
	reqMissingZone := &csi.CreateVolumeRequest{
		Name: "vol-no-zone",
		VolumeCapabilities: []*csi.VolumeCapability{
			accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		},
		Parameters: map[string]string{
			"type": "SSD",
		},
	}
	_, err = svc.CreateVolume(context.Background(), reqMissingZone)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for missing zone, got %v", err)
	}

	// Invalid capacity range
	reqBadCap := &csi.CreateVolumeRequest{
		Name: "vol-bad-cap",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 100 * 1024 * 1024 * 1024,
			LimitBytes:    10 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities: reqMissingZone.VolumeCapabilities,
		Parameters:         reqMissingZone.Parameters,
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{driver.TopologyZoneKey: "eu-de-01"}},
			},
		},
	}
	_, err = svc.CreateVolume(context.Background(), reqBadCap)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for required > limit bytes, got %v", err)
	}
}

func TestDeleteVolume(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc, _ := driver.NewControllerService(client, validTestConfig())

	// Create a volume directly in mock
	vol, _ := client.CreateVolume(context.Background(), evs.CreateVolumeOpts{
		Name:             "del-vol",
		Size:             1,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})

	// Delete existing volume
	_, err := svc.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: vol.ID,
	})
	if err != nil {
		t.Fatalf("unexpected error deleting volume: %v", err)
	}

	// Idempotent delete of already deleted volume -> expected OK
	_, err = svc.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: vol.ID,
	})
	if err != nil {
		t.Fatalf("unexpected error deleting already deleted volume: %v", err)
	}

	// Validation: empty volume ID
	_, err = svc.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty volume_id, got %v", err)
	}
}

func TestValidateVolumeCapabilities(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc, _ := driver.NewControllerService(client, validTestConfig())

	vol, _ := client.CreateVolume(context.Background(), evs.CreateVolumeOpts{
		Name:             "val-vol",
		Size:             1,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})

	validCaps := []*csi.VolumeCapability{
		accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}

	// Valid capabilities
	resp, err := svc.ValidateVolumeCapabilities(
		context.Background(),
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           vol.ID,
			VolumeCapabilities: validCaps,
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Confirmed == nil {
		t.Error("expected non-nil Confirmed for valid capability")
	}

	// Unsupported access mode
	invalidCaps := []*csi.VolumeCapability{
		accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	}
	resp, err = svc.ValidateVolumeCapabilities(
		context.Background(),
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           vol.ID,
			VolumeCapabilities: invalidCaps,
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Confirmed != nil {
		t.Error("expected nil Confirmed for unsupported access mode")
	}

	// Volume not found
	_, err = svc.ValidateVolumeCapabilities(
		context.Background(),
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           "non-existent-vol",
			VolumeCapabilities: validCaps,
		},
	)
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for non-existent volume, got %v", err)
	}
}

// requisiteZone describes a topology requirement fixing one availability zone.
func requisiteZone(zone string) *csi.TopologyRequirement {
	return &csi.TopologyRequirement{
		Requisite: []*csi.Topology{
			{Segments: map[string]string{driver.TopologyZoneKey: zone}},
		},
	}
}

// TestCreateVolume_AcceptsOnlyTheDeclaredParameters fixes the accepted StorageClass parameter set.
// The container orchestrator owns its own key namespace, so those keys are ignored rather than
// refused, and every other key is refused before any cloud effect.
func TestCreateVolume_AcceptsOnlyTheDeclaredParameters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		parameters map[string]string
		topology   *csi.TopologyRequirement
		wantCode   codes.Code
		wantType   string
		wantZone   string
	}{
		{
			name:       "type alone with topology",
			parameters: map[string]string{"type": "SSD"},
			topology:   requisiteZone("eu-de-01"),
			wantCode:   codes.OK,
			wantType:   "SSD",
			wantZone:   "eu-de-01",
		},
		{
			name: "availability_zone supplies the zone when topology does not",
			parameters: map[string]string{
				"type":              "SSD",
				"availability_zone": "eu-de-03",
			},
			wantCode: codes.OK,
			wantType: "SSD",
			wantZone: "eu-de-03",
		},
		{
			name: "orchestrator-reserved keys are ignored",
			parameters: map[string]string{
				"type":                                       "SAS",
				"csi.storage.k8s.io/pvc/name":                "my-claim",
				"csi.storage.k8s.io/pvc/namespace":           "default",
				"csi.storage.k8s.io/provisioner-secret-name": "unused",
			},
			topology: requisiteZone("eu-de-01"),
			wantCode: codes.OK,
			wantType: "SAS",
			wantZone: "eu-de-01",
		},
		{
			name:       "absent type is refused",
			parameters: map[string]string{"availability_zone": "eu-de-01"},
			topology:   requisiteZone("eu-de-01"),
			wantCode:   codes.InvalidArgument,
		},
		{
			name:       "nil parameters are refused",
			parameters: nil,
			topology:   requisiteZone("eu-de-01"),
			wantCode:   codes.InvalidArgument,
		},
		{
			name:       "blank type is refused",
			parameters: map[string]string{"type": "   "},
			topology:   requisiteZone("eu-de-01"),
			wantCode:   codes.InvalidArgument,
		},
		{
			name: "withdrawn volume_type alias is refused",
			parameters: map[string]string{
				"type":        "SSD",
				"volume_type": "SSD",
			},
			topology: requisiteZone("eu-de-01"),
			wantCode: codes.InvalidArgument,
		},
		{
			name: "withdrawn zone alias is refused",
			parameters: map[string]string{
				"type": "SSD",
				"zone": "eu-de-01",
			},
			topology: requisiteZone("eu-de-01"),
			wantCode: codes.InvalidArgument,
		},
		{
			name: "any other key is refused",
			parameters: map[string]string{
				"type":   "SSD",
				"fsType": "ext4",
			},
			topology: requisiteZone("eu-de-01"),
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newMockEVSClient()
			svc, err := driver.NewControllerService(client, validTestConfig())
			if err != nil {
				t.Fatalf("NewControllerService failed: %v", err)
			}

			_, err = svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name:                      "parameter-vol",
				VolumeCapabilities:        []*csi.VolumeCapability{mountCapability("")},
				Parameters:                tt.parameters,
				AccessibilityRequirements: tt.topology,
			})
			if status.Code(err) != tt.wantCode {
				t.Fatalf(
					"CreateVolume status = %v (%v), want %v",
					status.Code(err),
					err,
					tt.wantCode,
				)
			}
			if tt.wantCode != codes.OK {
				if len(client.volumes) != 0 {
					t.Errorf("a refused request created %d volumes, want 0", len(client.volumes))
				}
				return
			}

			if client.lastCreate.VolumeType != tt.wantType {
				t.Errorf(
					"EVS create volume type = %q, want %q",
					client.lastCreate.VolumeType,
					tt.wantType,
				)
			}
			if client.lastCreate.AvailabilityZone != tt.wantZone {
				t.Errorf(
					"EVS create zone = %q, want %q",
					client.lastCreate.AvailabilityZone,
					tt.wantZone,
				)
			}
		})
	}
}

// TestCreateVolume_RejectsAVolumeContentSource proves provisioning from a snapshot or an existing
// volume is refused, because neither capability is advertised.
func TestCreateVolume_RejectsAVolumeContentSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source *csi.VolumeContentSource
	}{
		{
			name: "snapshot source",
			source: &csi.VolumeContentSource{
				Type: &csi.VolumeContentSource_Snapshot{
					Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "snap-1"},
				},
			},
		},
		{
			name: "volume source",
			source: &csi.VolumeContentSource{
				Type: &csi.VolumeContentSource_Volume{
					Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "vol-1"},
				},
			},
		},
		{
			name:   "source without a type",
			source: &csi.VolumeContentSource{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newMockEVSClient()
			svc, err := driver.NewControllerService(client, validTestConfig())
			if err != nil {
				t.Fatalf("NewControllerService failed: %v", err)
			}

			_, err = svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name:                      "cloned-vol",
				VolumeCapabilities:        []*csi.VolumeCapability{mountCapability("")},
				Parameters:                map[string]string{"type": "SSD"},
				AccessibilityRequirements: requisiteZone("eu-de-01"),
				VolumeContentSource:       tt.source,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf(
					"CreateVolume status = %v (%v), want InvalidArgument",
					status.Code(err),
					err,
				)
			}
			if len(client.volumes) != 0 {
				t.Errorf("a refused request created %d volumes, want 0", len(client.volumes))
			}
		})
	}
}

// TestCreateVolume_ResolvesRequestedCapacity fixes the resolved EVS size. An omitted or smaller
// requested capacity resolves to the declared minimum data-volume size, a partial GiB rounds up, and
// no requested size is capped.
func TestCreateVolume_ResolvesRequestedCapacity(t *testing.T) {
	t.Parallel()

	const gib = int64(1024 * 1024 * 1024)

	tests := []struct {
		name        string
		capacity    *csi.CapacityRange
		wantSizeGiB int
	}{
		{
			name:        "omitted range resolves to the declared minimum",
			wantSizeGiB: 10,
		},
		{
			name:        "empty range resolves to the declared minimum",
			capacity:    &csi.CapacityRange{},
			wantSizeGiB: 10,
		},
		{
			name:        "below-minimum request resolves to the declared minimum",
			capacity:    &csi.CapacityRange{RequiredBytes: gib},
			wantSizeGiB: 10,
		},
		{
			name:        "one byte resolves to the declared minimum",
			capacity:    &csi.CapacityRange{RequiredBytes: 1},
			wantSizeGiB: 10,
		},
		{
			name:        "limit alone resolves to the declared minimum",
			capacity:    &csi.CapacityRange{LimitBytes: 40 * gib},
			wantSizeGiB: 10,
		},
		{
			name:        "whole GiB request passes through",
			capacity:    &csi.CapacityRange{RequiredBytes: 20 * gib},
			wantSizeGiB: 20,
		},
		{
			name:        "partial GiB request rounds up",
			capacity:    &csi.CapacityRange{RequiredBytes: 20*gib + 1},
			wantSizeGiB: 21,
		},
		{
			name:        "half GiB above the minimum rounds up",
			capacity:    &csi.CapacityRange{RequiredBytes: 10*gib + gib/2},
			wantSizeGiB: 11,
		},
		{
			name: "large request is not capped",
			capacity: &csi.CapacityRange{
				RequiredBytes: 32768 * gib,
			},
			wantSizeGiB: 32768,
		},
		{
			name: "satisfiable limit is honoured",
			capacity: &csi.CapacityRange{
				RequiredBytes: 15 * gib,
				LimitBytes:    20 * gib,
			},
			wantSizeGiB: 15,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newMockEVSClient()
			svc, err := driver.NewControllerService(client, validTestConfig())
			if err != nil {
				t.Fatalf("NewControllerService failed: %v", err)
			}

			resp, err := svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name:                      "capacity-vol",
				CapacityRange:             tt.capacity,
				VolumeCapabilities:        []*csi.VolumeCapability{mountCapability("")},
				Parameters:                map[string]string{"type": "SSD"},
				AccessibilityRequirements: requisiteZone("eu-de-01"),
			})
			if err != nil {
				t.Fatalf("CreateVolume failed: %v", err)
			}

			if client.lastCreate.Size != tt.wantSizeGiB {
				t.Errorf(
					"EVS create size = %d GiB, want %d GiB",
					client.lastCreate.Size,
					tt.wantSizeGiB,
				)
			}
			if want := int64(tt.wantSizeGiB) * gib; resp.GetVolume().GetCapacityBytes() != want {
				t.Errorf(
					"reported capacity = %d bytes, want %d bytes",
					resp.GetVolume().GetCapacityBytes(),
					want,
				)
			}
		})
	}
}

// TestCreateVolume_DistinguishesMalformedFromUnsatisfiableCapacity proves a malformed capacity range
// is refused as caller input while a well-formed range no supported size can satisfy receives the
// CSI-prescribed out-of-range status, so a caller can tell the two apart.
func TestCreateVolume_DistinguishesMalformedFromUnsatisfiableCapacity(t *testing.T) {
	t.Parallel()

	const gib = int64(1024 * 1024 * 1024)

	tests := []struct {
		name     string
		capacity *csi.CapacityRange
		wantCode codes.Code
	}{
		{
			name: "required above limit is malformed",
			capacity: &csi.CapacityRange{
				RequiredBytes: 20 * gib,
				LimitBytes:    10 * gib,
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "negative required bytes are malformed",
			capacity: &csi.CapacityRange{RequiredBytes: -1},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "negative limit bytes are malformed",
			capacity: &csi.CapacityRange{LimitBytes: -1},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "limit below the declared minimum is unsatisfiable",
			capacity: &csi.CapacityRange{LimitBytes: 5 * gib},
			wantCode: codes.OutOfRange,
		},
		{
			name: "resolved minimum above the limit is unsatisfiable",
			capacity: &csi.CapacityRange{
				RequiredBytes: gib,
				LimitBytes:    5 * gib,
			},
			wantCode: codes.OutOfRange,
		},
		{
			name: "limit between whole GiB sizes is unsatisfiable",
			capacity: &csi.CapacityRange{
				RequiredBytes: 20*gib + 1,
				LimitBytes:    20*gib + 2,
			},
			wantCode: codes.OutOfRange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newMockEVSClient()
			svc, err := driver.NewControllerService(client, validTestConfig())
			if err != nil {
				t.Fatalf("NewControllerService failed: %v", err)
			}

			_, err = svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name:                      "range-vol",
				CapacityRange:             tt.capacity,
				VolumeCapabilities:        []*csi.VolumeCapability{mountCapability("")},
				Parameters:                map[string]string{"type": "SSD"},
				AccessibilityRequirements: requisiteZone("eu-de-01"),
			})
			if status.Code(err) != tt.wantCode {
				t.Fatalf(
					"CreateVolume status = %v (%v), want %v",
					status.Code(err),
					err,
					tt.wantCode,
				)
			}
			if len(client.volumes) != 0 {
				t.Errorf("a refused request created %d volumes, want 0", len(client.volumes))
			}
		})
	}
}

// TestCreateVolume_ReflectsNoParameterIntoTheResponse proves the volume context stays empty and no
// operator-supplied parameter is echoed to the caller, so no consumer can begin depending on
// parameter reflection.
func TestCreateVolume_ReflectsNoParameterIntoTheResponse(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc, err := driver.NewControllerService(client, validTestConfig())
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	resp, err := svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "context-vol",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability("")},
		Parameters: map[string]string{
			"type":                        "SSD",
			"availability_zone":           "eu-de-03",
			"csi.storage.k8s.io/pvc/name": "my-claim",
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	if volumeContext := resp.GetVolume().GetVolumeContext(); len(volumeContext) != 0 {
		t.Errorf("volume context = %v, want empty", volumeContext)
	}
	if resp.GetVolume().GetContentSource() != nil {
		t.Errorf("content source = %v, want nil", resp.GetVolume().GetContentSource())
	}
}

func TestControllerService_GRPC_WireTransport(t *testing.T) {
	t.Parallel()

	evsMock := newMockEVSClient()
	svc, err := driver.NewControllerService(evsMock, validTestConfig())
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	client := newControllerClient(t, svc)

	// ControllerGetCapabilities over wire
	capsResp, err := client.ControllerGetCapabilities(
		context.Background(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("ControllerGetCapabilities over gRPC failed: %v", err)
	}
	if len(capsResp.GetCapabilities()) != 2 {
		t.Errorf("expected 2 capabilities over gRPC wire, got: %d", len(capsResp.GetCapabilities()))
	}

	// CreateVolume over wire
	createReq := &csi.CreateVolumeRequest{
		Name: "wire-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			mountCapability(""),
		},
		Parameters: map[string]string{
			"type": "SSD",
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{driver.TopologyZoneKey: "eu-de-01"}},
			},
		},
	}

	createResp, err := client.CreateVolume(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateVolume over gRPC failed: %v", err)
	}
	if createResp.GetVolume().GetVolumeId() == "" {
		t.Error("expected non-empty VolumeId over wire")
	}
	if createResp.GetVolume().GetCapacityBytes() != 10*1024*1024*1024 {
		t.Errorf("expected capacity 10GiB, got %d", createResp.GetVolume().GetCapacityBytes())
	}

	// ControllerPublishVolume over wire
	pubResp, err := client.ControllerPublishVolume(
		context.Background(),
		&csi.ControllerPublishVolumeRequest{
			VolumeId:         createResp.GetVolume().GetVolumeId(),
			NodeId:           "node-uuid-wire",
			VolumeCapability: createReq.GetVolumeCapabilities()[0],
		},
	)
	if err != nil {
		t.Fatalf("ControllerPublishVolume over gRPC failed: %v", err)
	}
	if pubResp == nil {
		t.Error("expected non-nil response for ControllerPublishVolume over wire")
	}

	// ControllerUnpublishVolume over wire
	_, err = client.ControllerUnpublishVolume(
		context.Background(),
		&csi.ControllerUnpublishVolumeRequest{
			VolumeId: createResp.GetVolume().GetVolumeId(),
			NodeId:   "node-uuid-wire",
		},
	)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume over gRPC failed: %v", err)
	}

	// Idempotent CreateVolume over wire
	createResp2, err := client.CreateVolume(context.Background(), createReq)
	if err != nil {
		t.Fatalf("idempotent CreateVolume over gRPC failed: %v", err)
	}
	if createResp2.GetVolume().GetVolumeId() != createResp.GetVolume().GetVolumeId() {
		t.Errorf(
			"expected identical volume ID over wire, got %s and %s",
			createResp.GetVolume().GetVolumeId(),
			createResp2.GetVolume().GetVolumeId(),
		)
	}

	// ValidateVolumeCapabilities over wire
	valResp, err := client.ValidateVolumeCapabilities(
		context.Background(),
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           createResp.GetVolume().GetVolumeId(),
			VolumeCapabilities: createReq.GetVolumeCapabilities(),
		},
	)
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities over gRPC failed: %v", err)
	}
	if valResp.GetConfirmed() == nil {
		t.Error("expected non-nil Confirmed over gRPC wire")
	}

	// DeleteVolume over wire
	_, err = client.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
	})
	if err != nil {
		t.Fatalf("DeleteVolume over gRPC failed: %v", err)
	}

	// Idempotent DeleteVolume over wire
	_, err = client.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
	})
	if err != nil {
		t.Fatalf("idempotent DeleteVolume over gRPC failed: %v", err)
	}
}

func TestControllerService_UnixDomainSocket_WireTransport(t *testing.T) {
	t.Parallel()

	sockPath := filepath.Join(t.TempDir(), "csi.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to listen on unix domain socket %s: %v", sockPath, err)
	}

	evsMock := newMockEVSClient()
	svc, err := driver.NewControllerService(evsMock, validTestConfig())
	if err != nil {
		_ = ln.Close()
		t.Fatalf("NewControllerService failed: %v", err)
	}

	server := grpc.NewServer()
	csi.RegisterControllerServer(server, svc)

	go func() {
		_ = server.Serve(ln)
	}()

	conn, err := grpc.NewClient(
		"unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		server.GracefulStop()
		t.Fatalf("failed to dial unix domain socket %s: %v", sockPath, err)
	}

	client := csi.NewControllerClient(conn)

	defer func() {
		_ = conn.Close()
		server.GracefulStop()
		_ = ln.Close()
	}()

	// Perform gRPC volume creation, publish, unpublish, and deletion over Unix domain socket
	createResp, err := client.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "uds-volume",
		VolumeCapabilities: []*csi.VolumeCapability{
			accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		},
		Parameters: map[string]string{
			"type": "SSD",
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{driver.TopologyZoneKey: "eu-de-02"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume over UDS failed: %v", err)
	}
	if createResp.GetVolume().GetVolumeId() == "" {
		t.Fatal("expected non-empty VolumeId over UDS")
	}

	_, err = client.ControllerPublishVolume(
		context.Background(),
		&csi.ControllerPublishVolumeRequest{
			VolumeId: createResp.GetVolume().GetVolumeId(),
			NodeId:   "server-uuid-uds",
			VolumeCapability: accessModeCapability(
				csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			),
		},
	)
	if err != nil {
		t.Fatalf("ControllerPublishVolume over UDS failed: %v", err)
	}

	_, err = client.ControllerUnpublishVolume(
		context.Background(),
		&csi.ControllerUnpublishVolumeRequest{
			VolumeId: createResp.GetVolume().GetVolumeId(),
			NodeId:   "server-uuid-uds",
		},
	)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume over UDS failed: %v", err)
	}

	_, err = client.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
	})
	if err != nil {
		t.Fatalf("DeleteVolume over UDS failed: %v", err)
	}
}
