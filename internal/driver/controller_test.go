package driver

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
	"google.golang.org/grpc/test/bufconn"

	"wilaris.dev/t-cloud-public-csi-drive/internal/config"
	"wilaris.dev/t-cloud-public-csi-drive/internal/evs"
)

const bufSize = 1024 * 1024

type mockEVSClient struct {
	volumes map[string]*evs.Volume
}

func newMockEVSClient() *mockEVSClient {
	return &mockEVSClient{
		volumes: make(map[string]*evs.Volume),
	}
}

func (m *mockEVSClient) CreateVolume(
	ctx context.Context,
	opts evs.CreateVolumeOpts,
) (*evs.Volume, error) {
	vol := &evs.Volume{
		ID:               fmt.Sprintf("vol-%s", opts.Name),
		Name:             opts.Name,
		Status:           "available",
		Size:             opts.Size,
		AvailabilityZone: opts.AvailabilityZone,
		VolumeType:       opts.VolumeType,
		Metadata:         opts.Metadata,
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

func (m *mockEVSClient) ListVolumes(
	ctx context.Context,
	opts evs.ListVolumeOpts,
) ([]evs.Volume, error) {
	var res []evs.Volume
	for _, vol := range m.volumes {
		if opts.Name != "" && vol.Name != opts.Name {
			continue
		}
		if opts.ID != "" && vol.ID != opts.ID {
			continue
		}
		res = append(res, *vol)
	}
	return res, nil
}

func (m *mockEVSClient) DeleteVolume(ctx context.Context, id string) error {
	if _, ok := m.volumes[id]; !ok {
		return fmt.Errorf("volume %s: %w", id, evs.ErrNotFound)
	}
	delete(m.volumes, id)
	return nil
}

func testConfig() *config.Config {
	return &config.Config{
		Endpoint:   "unix:///tmp/test.sock",
		NodeID:     "node-1",
		DriverName: "evs.csi.t-cloud.ti-services.io",
		Version:    "v0.1.0",
		AuthURL:    "https://iam.example.com/v3",
		AccessKey:  "ak",
		SecretKey:  "sk",
		ProjectID:  "proj-1",
		RegionName: "eu-de",
	}
}

func TestNewControllerService(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	cfg := testConfig()

	svc, err := NewControllerService(client, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}

	_, err = NewControllerService(nil, cfg)
	if err == nil {
		t.Error("expected error for nil client")
	}

	_, err = NewControllerService(client, nil)
	if err == nil {
		t.Error("expected error for nil config")
	}
}

func TestControllerGetCapabilities(t *testing.T) {
	t.Parallel()

	svc, _ := NewControllerService(newMockEVSClient(), testConfig())
	resp, err := svc.ControllerGetCapabilities(
		context.Background(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Capabilities) != 1 {
		t.Fatalf("expected 1 capability, got %d", len(resp.Capabilities))
	}
	cap := resp.Capabilities[0].GetRpc().GetType()
	if cap != csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME {
		t.Errorf("expected CREATE_DELETE_VOLUME capability, got %v", cap)
	}
}

func TestCreateVolume_HappyPath(t *testing.T) {
	t.Parallel()

	svc, _ := NewControllerService(newMockEVSClient(), testConfig())
	req := &csi.CreateVolumeRequest{
		Name: "test-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{},
				},
			},
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{
					Segments: map[string]string{
						TopologyZoneKey: "eu-de-01",
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
		resp.Volume.AccessibleTopology[0].Segments[TopologyZoneKey] != "eu-de-01" {
		t.Errorf("unexpected accessible topology: %+v", resp.Volume.AccessibleTopology)
	}
}

func TestCreateVolume_Idempotency(t *testing.T) {
	t.Parallel()

	svc, _ := NewControllerService(newMockEVSClient(), testConfig())
	req := &csi.CreateVolumeRequest{
		Name: "idempotent-vol",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 5 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{TopologyZoneKey: "eu-de-01"}},
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

	// Retry with larger capacity -> expect AlreadyExists
	largeReq := &csi.CreateVolumeRequest{
		Name: "idempotent-vol",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 50 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities:        req.VolumeCapabilities,
		AccessibilityRequirements: req.AccessibilityRequirements,
	}

	_, err = svc.CreateVolume(context.Background(), largeReq)
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("expected AlreadyExists error for conflicting size, got %v", err)
	}
}

func TestCreateVolume_UnsupportedAccessMode(t *testing.T) {
	t.Parallel()

	svc, _ := NewControllerService(newMockEVSClient(), testConfig())
	req := &csi.CreateVolumeRequest{
		Name: "multi-node-vol",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{TopologyZoneKey: "eu-de-01"}},
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

	svc, _ := NewControllerService(newMockEVSClient(), testConfig())

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
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
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
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{TopologyZoneKey: "eu-de-01"}},
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
	svc, _ := NewControllerService(client, testConfig())

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
	svc, _ := NewControllerService(client, testConfig())

	vol, _ := client.CreateVolume(context.Background(), evs.CreateVolumeOpts{
		Name:             "val-vol",
		Size:             1,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})

	validCaps := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
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
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
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

func setupGRPCControllerServer(
	t *testing.T,
	svc *ControllerService,
) (csi.ControllerClient, func()) {
	t.Helper()

	ln := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	csi.RegisterControllerServer(server, svc)

	go func() {
		if err := server.Serve(ln); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("gRPC server error: %v", err)
		}
	}()

	bufDialer := func(context.Context, string) (net.Conn, error) {
		return ln.Dial()
	}

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(bufDialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}

	client := csi.NewControllerClient(conn)

	cleanup := func() {
		_ = conn.Close()
		server.GracefulStop()
		_ = ln.Close()
	}

	return client, cleanup
}

func TestControllerService_GRPC_WireTransport(t *testing.T) {
	t.Parallel()

	evsMock := newMockEVSClient()
	svc, err := NewControllerService(evsMock, testConfig())
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	client, cleanup := setupGRPCControllerServer(t, svc)
	defer cleanup()

	// ControllerGetCapabilities over wire
	capsResp, err := client.ControllerGetCapabilities(
		context.Background(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("ControllerGetCapabilities over gRPC failed: %v", err)
	}
	if len(capsResp.GetCapabilities()) != 1 ||
		capsResp.GetCapabilities()[0].GetRpc().
			GetType() !=
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME {
		t.Errorf("unexpected capabilities over gRPC wire: %v", capsResp.GetCapabilities())
	}

	// CreateVolume over wire
	createReq := &csi.CreateVolumeRequest{
		Name: "wire-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10 * 1024 * 1024 * 1024,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{},
				},
			},
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{TopologyZoneKey: "eu-de-01"}},
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
	svc, err := NewControllerService(evsMock, testConfig())
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

	// Perform gRPC volume creation and deletion over Unix domain socket
	createResp, err := client.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "uds-volume",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{TopologyZoneKey: "eu-de-02"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume over UDS failed: %v", err)
	}
	if createResp.GetVolume().GetVolumeId() == "" {
		t.Fatal("expected non-empty VolumeId over UDS")
	}

	_, err = client.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: createResp.GetVolume().GetVolumeId(),
	})
	if err != nil {
		t.Fatalf("DeleteVolume over UDS failed: %v", err)
	}
}
