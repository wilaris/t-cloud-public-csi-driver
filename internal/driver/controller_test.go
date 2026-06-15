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
	"google.golang.org/protobuf/proto"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

// mockAttachedDeviceName is the host path the fake cloud reports for a successful attach.
const mockAttachedDeviceName = "/dev/vdb"

// mockEVSClient is an in-memory EVSClient. It records creates so tests can assert
// what the controller asked the cloud to provision.
type mockEVSClient struct {
	volumes     map[string]*evs.Volume
	attachments map[string]string
	lastCreate  evs.CreateVolumeOpts
	createCalls int
}

func newMockEVSClient() *mockEVSClient {
	return &mockEVSClient{
		volumes:     make(map[string]*evs.Volume),
		attachments: make(map[string]string),
	}
}

func (m *mockEVSClient) CreateVolume(
	_ context.Context,
	opts evs.CreateVolumeOpts,
) (*evs.Volume, error) {
	m.lastCreate = opts
	m.createCalls++
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

func (m *mockEVSClient) GetVolume(_ context.Context, id string) (*evs.Volume, error) {
	vol, ok := m.volumes[id]
	if !ok {
		return nil, fmt.Errorf("volume %s: %w", id, evs.ErrNotFound)
	}
	return vol, nil
}

func (m *mockEVSClient) DiscoverVolume(
	_ context.Context,
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

func (m *mockEVSClient) DeleteVolume(_ context.Context, id string) error {
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

func (m *mockEVSClient) AttachVolume(
	_ context.Context,
	volumeID, serverID string,
) (*evs.Attachment, error) {
	if volumeID == "" || serverID == "" {
		return nil, fmt.Errorf("attach volume: %w", evs.ErrInvalidArgument)
	}
	vol, ok := m.volumes[volumeID]
	if !ok {
		return nil, fmt.Errorf("volume %s: %w", volumeID, evs.ErrNotFound)
	}
	if currentServer, attached := m.attachments[volumeID]; attached {
		if currentServer == serverID {
			return m.attachmentOf(volumeID, serverID), nil
		}
		return nil, fmt.Errorf(
			"volume %s attached to %s: %w",
			volumeID,
			currentServer,
			evs.ErrConflict,
		)
	}
	m.attachments[volumeID] = serverID
	vol.Status = "in-use"
	return m.attachmentOf(volumeID, serverID), nil
}

func (m *mockEVSClient) attachmentOf(volumeID, serverID string) *evs.Attachment {
	return &evs.Attachment{
		VolumeID:   volumeID,
		ServerID:   serverID,
		DeviceName: mockAttachedDeviceName,
	}
}

func (m *mockEVSClient) DetachVolume(_ context.Context, volumeID, serverID string) error {
	if volumeID == "" || serverID == "" {
		return fmt.Errorf("detach volume: %w", evs.ErrInvalidArgument)
	}
	// A deleted volume has no attachment on this server, so detach is already done.
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

// bareDeviceEVSClient attaches successfully but names a device the node cannot open.
type bareDeviceEVSClient struct {
	*mockEVSClient
}

func (c *bareDeviceEVSClient) AttachVolume(
	ctx context.Context,
	volumeID, serverID string,
) (*evs.Attachment, error) {
	attachment, err := c.mockEVSClient.AttachVolume(ctx, volumeID, serverID)
	if err != nil {
		return nil, err
	}
	attachment.DeviceName = "vdb"
	return attachment, nil
}

func mustController(t *testing.T, client driver.EVSClient) *driver.ControllerService {
	t.Helper()
	svc, err := driver.NewControllerService(client, validTestConfig(), discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService() = %v", err)
	}
	return svc
}

func mustSeedVolume(t *testing.T, client *mockEVSClient, name string) *evs.Volume {
	t.Helper()
	vol, err := client.CreateVolume(t.Context(), evs.CreateVolumeOpts{
		Name:             name,
		Size:             1,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})
	if err != nil {
		t.Fatalf("CreateVolume(%q) = %v", name, err)
	}
	return vol
}

// requisiteZone pins CreateVolume to a single availability zone.
func requisiteZone(zone string) *csi.TopologyRequirement {
	return &csi.TopologyRequirement{
		Requisite: []*csi.Topology{
			{Segments: map[string]string{driver.TopologyZoneKey: zone}},
		},
	}
}

func TestNewControllerService(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	cfg := validTestConfig()

	svc, err := driver.NewControllerService(client, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService() = %v", err)
	}
	if svc == nil {
		t.Fatal("NewControllerService() = nil, want service")
	}

	_, err = driver.NewControllerService(nil, cfg, discardLogger())
	if err == nil {
		t.Error("NewControllerService(nil client) = nil, want error")
	}

	_, err = driver.NewControllerService(client, nil, discardLogger())
	if err == nil {
		t.Error("NewControllerService(nil config) = nil, want error")
	}

	_, err = driver.NewControllerService(client, cfg, nil)
	if err == nil {
		t.Error("NewControllerService(nil logger) = nil, want error")
	}
}

func TestControllerGetCapabilities(t *testing.T) {
	t.Parallel()

	svc := mustController(t, newMockEVSClient())
	resp, err := svc.ControllerGetCapabilities(
		t.Context(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("ControllerGetCapabilities() = %v", err)
	}
	if len(resp.Capabilities) != 2 {
		t.Fatalf("ControllerGetCapabilities() len = %d, want 2", len(resp.Capabilities))
	}
	cap0 := resp.Capabilities[0].GetRpc().GetType()
	if cap0 != csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME {
		t.Errorf(
			"ControllerGetCapabilities()[0] = %v, want CREATE_DELETE_VOLUME",
			cap0,
		)
	}
	cap1 := resp.Capabilities[1].GetRpc().GetType()
	if cap1 != csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME {
		t.Errorf(
			"ControllerGetCapabilities()[1] = %v, want PUBLISH_UNPUBLISH_VOLUME",
			cap1,
		)
	}
}

func TestControllerPublishVolume_HappyPath(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc := mustController(t, client)
	vol := mustSeedVolume(t, client, "pub-vol")

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         vol.ID,
		NodeId:           "server-uuid-1234",
		VolumeCapability: accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
	resp, err := svc.ControllerPublishVolume(t.Context(), req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume() = %v", err)
	}
	if resp == nil {
		t.Fatal("ControllerPublishVolume() = nil, want response")
	}
	if got := resp.GetPublishContext()["devicePath"]; got != mockAttachedDeviceName {
		t.Errorf(
			"ControllerPublishVolume() devicePath = %q, want %q",
			got,
			mockAttachedDeviceName,
		)
	}

	updated, err := client.GetVolume(t.Context(), vol.ID)
	if err != nil {
		t.Fatalf("GetVolume(%q) = %v", vol.ID, err)
	}
	if updated.Status != "in-use" {
		t.Errorf("GetVolume().Status = %q, want in-use", updated.Status)
	}
}

func TestControllerPublishVolume_UnusableDevicePath(t *testing.T) {
	t.Parallel()

	// A successful attach that names "vdb" is unusable on the node, so the
	// controller must report Internal instead of returning that path as OK.
	client := newMockEVSClient()
	svc := mustController(t, &bareDeviceEVSClient{client})
	vol := mustSeedVolume(t, client, "bare-device-vol")

	_, err := svc.ControllerPublishVolume(t.Context(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         vol.ID,
		NodeId:           "server-uuid-1234",
		VolumeCapability: accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	if status.Code(err) != codes.Internal {
		t.Errorf("ControllerPublishVolume() status = %v, want Internal", err)
	}
}

func TestControllerPublishVolume_Idempotency(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc := mustController(t, client)
	vol := mustSeedVolume(t, client, "idempotent-pub-vol")

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         vol.ID,
		NodeId:           "server-uuid-1234",
		VolumeCapability: accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
	if _, err := svc.ControllerPublishVolume(t.Context(), req); err != nil {
		t.Fatalf("ControllerPublishVolume() first = %v", err)
	}
	if _, err := svc.ControllerPublishVolume(t.Context(), req); err != nil {
		t.Fatalf("ControllerPublishVolume() second = %v", err)
	}

	// Already attached elsewhere: the CO must unpublish the other node first.
	_, err := svc.ControllerPublishVolume(t.Context(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         vol.ID,
		NodeId:           "server-uuid-5678",
		VolumeCapability: req.VolumeCapability,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ControllerPublishVolume(other node) status = %v, want FailedPrecondition", err)
	}
}

func TestControllerPublishVolume_NonexistentVolume(t *testing.T) {
	t.Parallel()

	svc := mustController(t, newMockEVSClient())
	_, err := svc.ControllerPublishVolume(t.Context(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         "vol-missing",
		NodeId:           "server-uuid-1234",
		VolumeCapability: accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("ControllerPublishVolume(missing) status = %v, want NotFound", err)
	}
}

func TestControllerPublishVolume_ValidationFailures(t *testing.T) {
	t.Parallel()

	svc := mustController(t, newMockEVSClient())
	writer := accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)

	tests := []struct {
		name string
		req  *csi.ControllerPublishVolumeRequest
	}{
		{name: "nil request"},
		{
			name: "empty volume_id",
			req: &csi.ControllerPublishVolumeRequest{
				NodeId:           "node-1",
				VolumeCapability: writer,
			},
		},
		{
			name: "empty node_id",
			req: &csi.ControllerPublishVolumeRequest{
				VolumeId:         "vol-1",
				VolumeCapability: writer,
			},
		},
		{
			name: "nil capability",
			req: &csi.ControllerPublishVolumeRequest{
				VolumeId: "vol-1",
				NodeId:   "node-1",
			},
		},
		{
			name: "multi-node mode",
			req: &csi.ControllerPublishVolumeRequest{
				VolumeId: "vol-1",
				NodeId:   "node-1",
				VolumeCapability: accessModeCapability(
					csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := svc.ControllerPublishVolume(t.Context(), tt.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf(
					"ControllerPublishVolume(%s) status = %v, want InvalidArgument",
					tt.name,
					err,
				)
			}
		})
	}
}

func TestControllerUnpublishVolume_HappyPath(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc := mustController(t, client)
	vol := mustSeedVolume(t, client, "unpub-vol")

	if _, err := client.AttachVolume(t.Context(), vol.ID, "server-uuid-1234"); err != nil {
		t.Fatalf("AttachVolume() = %v", err)
	}

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: vol.ID,
		NodeId:   "server-uuid-1234",
	}
	resp, err := svc.ControllerUnpublishVolume(t.Context(), req)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume() = %v", err)
	}
	if resp == nil {
		t.Fatal("ControllerUnpublishVolume() = nil, want response")
	}
	if _, err := svc.ControllerUnpublishVolume(t.Context(), req); err != nil {
		t.Fatalf("ControllerUnpublishVolume() second = %v", err)
	}
}

func TestControllerUnpublishVolume_AfterDelete(t *testing.T) {
	t.Parallel()

	// After DeleteVolume the volume is gone; Unpublish must still succeed so
	// the CO can finish cleanup without treating absence as a failure.
	client := newMockEVSClient()
	svc := mustController(t, client)
	vol := mustSeedVolume(t, client, "unpub-after-delete-vol")

	_, err := svc.DeleteVolume(t.Context(), &csi.DeleteVolumeRequest{VolumeId: vol.ID})
	if err != nil {
		t.Fatalf("DeleteVolume() = %v", err)
	}

	_, err = svc.ControllerUnpublishVolume(t.Context(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: vol.ID,
		NodeId:   "server-uuid-1234",
	})
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume() after delete = %v, want success", err)
	}
}

func TestControllerUnpublishVolume_ValidationFailures(t *testing.T) {
	t.Parallel()

	svc := mustController(t, newMockEVSClient())

	tests := []struct {
		name string
		req  *csi.ControllerUnpublishVolumeRequest
	}{
		{name: "nil request"},
		{
			name: "empty volume_id",
			req:  &csi.ControllerUnpublishVolumeRequest{NodeId: "node-1"},
		},
		{
			name: "empty node_id",
			req:  &csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := svc.ControllerUnpublishVolume(t.Context(), tt.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf(
					"ControllerUnpublishVolume(%s) status = %v, want InvalidArgument",
					tt.name,
					err,
				)
			}
		})
	}
}

func TestCreateVolume_HappyPath(t *testing.T) {
	t.Parallel()

	svc := mustController(t, newMockEVSClient())
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

	resp, err := svc.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatalf("CreateVolume() = %v", err)
	}
	if resp.Volume.VolumeId == "" {
		t.Error("CreateVolume() VolumeId is empty")
	}
	if resp.Volume.CapacityBytes != 10*1024*1024*1024 {
		t.Errorf(
			"CreateVolume() CapacityBytes = %d, want 10GiB",
			resp.Volume.CapacityBytes,
		)
	}
	if len(resp.Volume.AccessibleTopology) != 1 ||
		resp.Volume.AccessibleTopology[0].Segments[driver.TopologyZoneKey] != "eu-de-01" {
		t.Errorf(
			"CreateVolume() AccessibleTopology = %+v, want eu-de-01",
			resp.Volume.AccessibleTopology,
		)
	}
}

func TestCreateVolume_TopologyRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	identityService, err := driver.NewIdentityService(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewIdentityService() = %v", err)
	}
	nodeService, err := driver.NewNodeService(&fakeMounter{}, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewNodeService() = %v", err)
	}
	evsClient := newMockEVSClient()
	controllerService, err := driver.NewControllerService(evsClient, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService() = %v", err)
	}

	conn := serveCSI(t, func(server *grpc.Server) {
		csi.RegisterIdentityServer(server, identityService)
		csi.RegisterNodeServer(server, nodeService)
		csi.RegisterControllerServer(server, controllerService)
	})

	identityClient := csi.NewIdentityClient(conn)
	capabilities, err := identityClient.GetPluginCapabilities(
		t.Context(),
		&csi.GetPluginCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("GetPluginCapabilities() = %v", err)
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
		t.Fatal("GetPluginCapabilities() missing VOLUME_ACCESSIBILITY_CONSTRAINTS")
	}

	nodeClient := csi.NewNodeClient(conn)
	nodeInfo, err := nodeClient.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo() = %v", err)
	}
	nodeTopology := nodeInfo.GetAccessibleTopology()
	if nodeTopology == nil {
		t.Fatal("NodeGetInfo() AccessibleTopology = nil")
	}
	nodeZone, ok := nodeTopology.GetSegments()[driver.TopologyZoneKey]
	if !ok {
		t.Fatalf("NodeGetInfo() topology missing %q", driver.TopologyZoneKey)
	}
	if nodeZone != cfg.AvailabilityZone {
		t.Fatalf("NodeGetInfo() zone = %q, want %q", nodeZone, cfg.AvailabilityZone)
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
		t.Fatalf("CreateVolume request zone = %q, want %q", requestZone, nodeZone)
	}

	created, err := controllerClient.CreateVolume(t.Context(), request)
	if err != nil {
		t.Fatalf("CreateVolume() = %v", err)
	}
	if evsClient.lastCreate.AvailabilityZone != requestZone {
		t.Fatalf(
			"EVS create zone = %q, want %q",
			evsClient.lastCreate.AvailabilityZone,
			requestZone,
		)
	}

	accessibleTopology := created.GetVolume().GetAccessibleTopology()
	if len(accessibleTopology) != 1 {
		t.Fatalf("CreateVolume() topology len = %d, want 1", len(accessibleTopology))
	}
	createdZone, ok := accessibleTopology[0].GetSegments()[driver.TopologyZoneKey]
	if !ok {
		t.Fatalf("CreateVolume() topology missing %q", driver.TopologyZoneKey)
	}
	if createdZone != evsClient.lastCreate.AvailabilityZone {
		t.Errorf(
			"CreateVolume() zone = %q, want %q",
			createdZone,
			evsClient.lastCreate.AvailabilityZone,
		)
	}
}

func TestCreateVolume_Idempotency(t *testing.T) {
	t.Parallel()

	evsClient := newMockEVSClient()
	svc := mustController(t, evsClient)
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

	resp1, err := svc.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatalf("CreateVolume() first = %v", err)
	}
	resp2, err := svc.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatalf("CreateVolume() second = %v", err)
	}
	if !proto.Equal(resp1.Volume, resp2.Volume) {
		t.Error("CreateVolume() repeated volume contract differs")
	}
	if evsClient.createCalls != 1 {
		t.Errorf("CreateVolume() EVS creates = %d, want 1", evsClient.createCalls)
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
	resp3, err := svc.CreateVolume(t.Context(), compatibleReq)
	if err != nil {
		t.Fatalf("CreateVolume() compatible retry = %v", err)
	}
	if resp3.Volume.VolumeId != resp1.Volume.VolumeId {
		t.Errorf(
			"CreateVolume() compatible VolumeId = %s, want %s",
			resp3.Volume.VolumeId,
			resp1.Volume.VolumeId,
		)
	}
	if evsClient.createCalls != 1 {
		t.Errorf("CreateVolume() compatible EVS creates = %d, want 1", evsClient.createCalls)
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
			_, err := client.CreateVolume(t.Context(), evs.CreateVolumeOpts{
				Name:             "existing-volume",
				Size:             tt.sizeGiB,
				AvailabilityZone: tt.zone,
				VolumeType:       tt.volumeType,
			})
			if err != nil {
				t.Fatalf("CreateVolume(existing) = %v", err)
			}

			svc := mustController(t, client)
			_, err = svc.CreateVolume(t.Context(), &csi.CreateVolumeRequest{
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
				t.Errorf(
					"CreateVolume(%s) status = %v, want AlreadyExists",
					tt.name,
					err,
				)
			}
		})
	}
}

func TestCreateVolume_UnsupportedAccessMode(t *testing.T) {
	t.Parallel()

	svc := mustController(t, newMockEVSClient())
	_, err := svc.CreateVolume(t.Context(), &csi.CreateVolumeRequest{
		Name: "multi-node-vol",
		VolumeCapabilities: []*csi.VolumeCapability{
			accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{driver.TopologyZoneKey: "eu-de-01"}},
			},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateVolume(multi-node) status = %v, want InvalidArgument", err)
	}
}

func TestCreateVolume_ValidationFailures(t *testing.T) {
	t.Parallel()

	svc := mustController(t, newMockEVSClient())
	writer := []*csi.VolumeCapability{
		accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
	params := map[string]string{"type": "SSD"}

	tests := []struct {
		name string
		req  *csi.CreateVolumeRequest
	}{
		{name: "nil request"},
		{
			name: "empty name",
			req:  &csi.CreateVolumeRequest{},
		},
		{
			name: "empty capabilities",
			req:  &csi.CreateVolumeRequest{Name: "vol-1"},
		},
		{
			name: "missing zone",
			req: &csi.CreateVolumeRequest{
				Name:               "vol-no-zone",
				VolumeCapabilities: writer,
				Parameters:         params,
			},
		},
		{
			name: "required above limit",
			req: &csi.CreateVolumeRequest{
				Name: "vol-bad-cap",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: 100 * 1024 * 1024 * 1024,
					LimitBytes:    10 * 1024 * 1024 * 1024,
				},
				VolumeCapabilities:        writer,
				Parameters:                params,
				AccessibilityRequirements: requisiteZone("eu-de-01"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := svc.CreateVolume(t.Context(), tt.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf(
					"CreateVolume(%s) status = %v, want InvalidArgument",
					tt.name,
					err,
				)
			}
		})
	}
}

func TestDeleteVolume(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc := mustController(t, client)
	vol := mustSeedVolume(t, client, "del-vol")

	_, err := svc.DeleteVolume(t.Context(), &csi.DeleteVolumeRequest{
		VolumeId: vol.ID,
	})
	if err != nil {
		t.Fatalf("DeleteVolume() = %v", err)
	}

	_, err = svc.DeleteVolume(t.Context(), &csi.DeleteVolumeRequest{
		VolumeId: vol.ID,
	})
	if err != nil {
		t.Fatalf("DeleteVolume() already deleted = %v", err)
	}

	_, err = svc.DeleteVolume(t.Context(), &csi.DeleteVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("DeleteVolume(empty id) status = %v, want InvalidArgument", err)
	}
}

func TestValidateVolumeCapabilities(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc := mustController(t, client)
	vol := mustSeedVolume(t, client, "val-vol")

	validCaps := []*csi.VolumeCapability{
		accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
	resp, err := svc.ValidateVolumeCapabilities(
		t.Context(),
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           vol.ID,
			VolumeCapabilities: validCaps,
		},
	)
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v", err)
	}
	if resp.Confirmed == nil {
		t.Error("ValidateVolumeCapabilities() Confirmed = nil, want set")
	}

	invalidCaps := []*csi.VolumeCapability{
		accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	}
	resp, err = svc.ValidateVolumeCapabilities(
		t.Context(),
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           vol.ID,
			VolumeCapabilities: invalidCaps,
		},
	)
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities(multi-node) = %v", err)
	}
	if resp.Confirmed != nil {
		t.Error("ValidateVolumeCapabilities(multi-node) Confirmed set, want nil")
	}

	_, err = svc.ValidateVolumeCapabilities(
		t.Context(),
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           "non-existent-vol",
			VolumeCapabilities: validCaps,
		},
	)
	if status.Code(err) != codes.NotFound {
		t.Errorf("ValidateVolumeCapabilities(missing) status = %v, want NotFound", err)
	}
}

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
			name:       "absent type is rejected",
			parameters: map[string]string{"availability_zone": "eu-de-01"},
			topology:   requisiteZone("eu-de-01"),
			wantCode:   codes.InvalidArgument,
		},
		{
			name:       "nil parameters are rejected",
			parameters: nil,
			topology:   requisiteZone("eu-de-01"),
			wantCode:   codes.InvalidArgument,
		},
		{
			name:       "blank type is rejected",
			parameters: map[string]string{"type": "   "},
			topology:   requisiteZone("eu-de-01"),
			wantCode:   codes.InvalidArgument,
		},
		{
			name: "withdrawn volume_type alias is rejected",
			parameters: map[string]string{
				"type":        "SSD",
				"volume_type": "SSD",
			},
			topology: requisiteZone("eu-de-01"),
			wantCode: codes.InvalidArgument,
		},
		{
			name: "withdrawn zone alias is rejected",
			parameters: map[string]string{
				"type": "SSD",
				"zone": "eu-de-01",
			},
			topology: requisiteZone("eu-de-01"),
			wantCode: codes.InvalidArgument,
		},
		{
			name: "any other key is rejected",
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
			svc := mustController(t, client)
			_, err := svc.CreateVolume(t.Context(), &csi.CreateVolumeRequest{
				Name:                      "parameter-vol",
				VolumeCapabilities:        []*csi.VolumeCapability{mountCapability("")},
				Parameters:                tt.parameters,
				AccessibilityRequirements: tt.topology,
			})
			if status.Code(err) != tt.wantCode {
				t.Fatalf(
					"CreateVolume() status = %v (%v), want %v",
					status.Code(err),
					err,
					tt.wantCode,
				)
			}
			if tt.wantCode != codes.OK {
				if len(client.volumes) != 0 {
					t.Errorf("CreateVolume() volumes = %d, want 0", len(client.volumes))
				}
				return
			}
			if client.lastCreate.VolumeType != tt.wantType {
				t.Errorf(
					"CreateVolume() EVS type = %q, want %q",
					client.lastCreate.VolumeType,
					tt.wantType,
				)
			}
			if client.lastCreate.AvailabilityZone != tt.wantZone {
				t.Errorf(
					"CreateVolume() EVS zone = %q, want %q",
					client.lastCreate.AvailabilityZone,
					tt.wantZone,
				)
			}
		})
	}
}

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
			svc := mustController(t, client)
			_, err := svc.CreateVolume(t.Context(), &csi.CreateVolumeRequest{
				Name:                      "cloned-vol",
				VolumeCapabilities:        []*csi.VolumeCapability{mountCapability("")},
				Parameters:                map[string]string{"type": "SSD"},
				AccessibilityRequirements: requisiteZone("eu-de-01"),
				VolumeContentSource:       tt.source,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf(
					"CreateVolume() status = %v (%v), want InvalidArgument",
					status.Code(err),
					err,
				)
			}
			if len(client.volumes) != 0 {
				t.Errorf("CreateVolume() volumes = %d, want 0", len(client.volumes))
			}
		})
	}
}

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
			name: "satisfiable limit is honored",
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
			svc := mustController(t, client)
			resp, err := svc.CreateVolume(t.Context(), &csi.CreateVolumeRequest{
				Name:                      "capacity-vol",
				CapacityRange:             tt.capacity,
				VolumeCapabilities:        []*csi.VolumeCapability{mountCapability("")},
				Parameters:                map[string]string{"type": "SSD"},
				AccessibilityRequirements: requisiteZone("eu-de-01"),
			})
			if err != nil {
				t.Fatalf("CreateVolume() = %v", err)
			}
			if client.lastCreate.Size != tt.wantSizeGiB {
				t.Errorf(
					"CreateVolume() EVS size = %d GiB, want %d GiB",
					client.lastCreate.Size,
					tt.wantSizeGiB,
				)
			}
			wantBytes := int64(tt.wantSizeGiB) * gib
			if resp.GetVolume().GetCapacityBytes() != wantBytes {
				t.Errorf(
					"CreateVolume() CapacityBytes = %d, want %d",
					resp.GetVolume().GetCapacityBytes(),
					wantBytes,
				)
			}
		})
	}
}

func TestCreateVolume_DistinguishesMalformedFromUnsatisfiableCapacity(t *testing.T) {
	t.Parallel()

	// A range that cannot be a request is InvalidArgument. A well-formed
	// range that no whole-GiB size can meet is OutOfRange.
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
			svc := mustController(t, client)
			_, err := svc.CreateVolume(t.Context(), &csi.CreateVolumeRequest{
				Name:                      "range-vol",
				CapacityRange:             tt.capacity,
				VolumeCapabilities:        []*csi.VolumeCapability{mountCapability("")},
				Parameters:                map[string]string{"type": "SSD"},
				AccessibilityRequirements: requisiteZone("eu-de-01"),
			})
			if status.Code(err) != tt.wantCode {
				t.Fatalf(
					"CreateVolume() status = %v (%v), want %v",
					status.Code(err),
					err,
					tt.wantCode,
				)
			}
			if len(client.volumes) != 0 {
				t.Errorf("CreateVolume() volumes = %d, want 0", len(client.volumes))
			}
		})
	}
}

func TestCreateVolume_ReflectsNoParameterIntoTheResponse(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc := mustController(t, client)
	resp, err := svc.CreateVolume(t.Context(), &csi.CreateVolumeRequest{
		Name:               "context-vol",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability("")},
		Parameters: map[string]string{
			"type":                        "SSD",
			"availability_zone":           "eu-de-03",
			"csi.storage.k8s.io/pvc/name": "my-claim",
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume() = %v", err)
	}
	if volumeContext := resp.GetVolume().GetVolumeContext(); len(volumeContext) != 0 {
		t.Errorf("CreateVolume() VolumeContext = %v, want empty", volumeContext)
	}
	if resp.GetVolume().GetContentSource() != nil {
		t.Errorf(
			"CreateVolume() ContentSource = %v, want nil",
			resp.GetVolume().GetContentSource(),
		)
	}
}

func TestControllerService_GRPC_WireTransport(t *testing.T) {
	t.Parallel()

	evsMock := newMockEVSClient()
	svc := mustController(t, evsMock)
	client := newControllerClient(t, svc)

	capsResp, err := client.ControllerGetCapabilities(
		t.Context(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("ControllerGetCapabilities() = %v", err)
	}
	if len(capsResp.GetCapabilities()) != 2 {
		t.Errorf(
			"ControllerGetCapabilities() len = %d, want 2",
			len(capsResp.GetCapabilities()),
		)
	}

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
	createResp, err := client.CreateVolume(t.Context(), createReq)
	if err != nil {
		t.Fatalf("CreateVolume() = %v", err)
	}
	if createResp.GetVolume().GetVolumeId() == "" {
		t.Error("CreateVolume() VolumeId is empty")
	}
	if createResp.GetVolume().GetCapacityBytes() != 10*1024*1024*1024 {
		t.Errorf(
			"CreateVolume() CapacityBytes = %d, want 10GiB",
			createResp.GetVolume().GetCapacityBytes(),
		)
	}

	pubResp, err := client.ControllerPublishVolume(
		t.Context(),
		&csi.ControllerPublishVolumeRequest{
			VolumeId:         createResp.GetVolume().GetVolumeId(),
			NodeId:           "node-uuid-wire",
			VolumeCapability: createReq.GetVolumeCapabilities()[0],
		},
	)
	if err != nil {
		t.Fatalf("ControllerPublishVolume() = %v", err)
	}
	if pubResp == nil {
		t.Error("ControllerPublishVolume() = nil, want response")
	}

	_, err = client.ControllerUnpublishVolume(
		t.Context(),
		&csi.ControllerUnpublishVolumeRequest{
			VolumeId: createResp.GetVolume().GetVolumeId(),
			NodeId:   "node-uuid-wire",
		},
	)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume() = %v", err)
	}

	createResp2, err := client.CreateVolume(t.Context(), createReq)
	if err != nil {
		t.Fatalf("CreateVolume() idempotent = %v", err)
	}
	if createResp2.GetVolume().GetVolumeId() != createResp.GetVolume().GetVolumeId() {
		t.Errorf(
			"CreateVolume() VolumeId = %s, want %s",
			createResp2.GetVolume().GetVolumeId(),
			createResp.GetVolume().GetVolumeId(),
		)
	}

	valResp, err := client.ValidateVolumeCapabilities(
		t.Context(),
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           createResp.GetVolume().GetVolumeId(),
			VolumeCapabilities: createReq.GetVolumeCapabilities(),
		},
	)
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v", err)
	}
	if valResp.GetConfirmed() == nil {
		t.Error("ValidateVolumeCapabilities() Confirmed = nil, want set")
	}

	_, err = client.DeleteVolume(t.Context(), &csi.DeleteVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
	})
	if err != nil {
		t.Fatalf("DeleteVolume() = %v", err)
	}

	_, err = client.DeleteVolume(t.Context(), &csi.DeleteVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
	})
	if err != nil {
		t.Fatalf("DeleteVolume() idempotent = %v", err)
	}
}

func TestControllerService_UnixDomainSocket_WireTransport(t *testing.T) {
	t.Parallel()

	sockPath := filepath.Join(t.TempDir(), "csi.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(unix, %s) = %v", sockPath, err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	evsMock := newMockEVSClient()
	svc := mustController(t, evsMock)

	server := grpc.NewServer()
	csi.RegisterControllerServer(server, svc)
	t.Cleanup(server.GracefulStop)

	go func() {
		serveErr := server.Serve(ln)
		if serveErr != nil && serveErr != grpc.ErrServerStopped {
			t.Errorf("Serve() = %v", serveErr)
		}
	}()

	conn, err := grpc.NewClient(
		"unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient(%s) = %v", sockPath, err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	client := csi.NewControllerClient(conn)
	createResp, err := client.CreateVolume(t.Context(), &csi.CreateVolumeRequest{
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
		t.Fatalf("CreateVolume() = %v", err)
	}
	if createResp.GetVolume().GetVolumeId() == "" {
		t.Fatal("CreateVolume() VolumeId is empty")
	}

	_, err = client.ControllerPublishVolume(
		t.Context(),
		&csi.ControllerPublishVolumeRequest{
			VolumeId: createResp.GetVolume().GetVolumeId(),
			NodeId:   "server-uuid-uds",
			VolumeCapability: accessModeCapability(
				csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			),
		},
	)
	if err != nil {
		t.Fatalf("ControllerPublishVolume() = %v", err)
	}

	_, err = client.ControllerUnpublishVolume(
		t.Context(),
		&csi.ControllerUnpublishVolumeRequest{
			VolumeId: createResp.GetVolume().GetVolumeId(),
			NodeId:   "server-uuid-uds",
		},
	)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume() = %v", err)
	}

	_, err = client.DeleteVolume(t.Context(), &csi.DeleteVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
	})
	if err != nil {
		t.Fatalf("DeleteVolume() = %v", err)
	}
}
