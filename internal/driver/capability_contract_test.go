package driver_test

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

// mountedCapability builds a mount capability; empty fsType means the fs field is omitted.
func mountedCapability(
	mode csi.VolumeCapability_AccessMode_Mode,
	fsType string,
) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fsType},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

// rawBlockCapability describes a raw block volume with an explicit access mode.
func rawBlockCapability(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

// allAccessModes lists every CSI access mode so a new mode from a spec bump is covered.
func allAccessModes() []csi.VolumeCapability_AccessMode_Mode {
	names := csi.VolumeCapability_AccessMode_Mode_name
	modes := make([]csi.VolumeCapability_AccessMode_Mode, 0, len(names))
	for value := range names {
		modes = append(modes, csi.VolumeCapability_AccessMode_Mode(value))
	}
	slices.Sort(modes)

	return modes
}

// recordingMounter counts host ops; failures never come from the mounter.
type recordingMounter struct {
	discoverDevice int
	formatAndMount int
	mounts         []recordedMount
}

func (m *recordingMounter) DiscoverDevice(
	ctx context.Context,
	volumeID, publishedDevicePath string,
) (string, error) {
	m.discoverDevice++

	return "/dev/sdb", nil
}

func (m *recordingMounter) FormatAndMount(
	ctx context.Context,
	source, target, fsType string,
	options []string,
) error {
	m.formatAndMount++

	return nil
}

func (m *recordingMounter) Mount(
	ctx context.Context,
	source, target, fsType string,
	options []string,
) error {
	m.mounts = append(m.mounts, recordedMount{
		source:  source,
		target:  target,
		fsType:  fsType,
		options: options,
	})

	return nil
}

func (m *recordingMounter) Unmount(ctx context.Context, target string) error {
	return nil
}

func (m *recordingMounter) IsMountPoint(ctx context.Context, target string) (bool, error) {
	return true, nil
}

func (m *recordingMounter) GetFilesystemType(ctx context.Context, source string) (string, error) {
	return "ext4", nil
}

func (m *recordingMounter) GetMountSource(ctx context.Context, target string) (string, error) {
	return "/dev/sdb", nil
}

// capabilityRPC names one CSI RPC that runs the shared volume-capability validator.
type capabilityRPC string

const (
	rpcCreateVolume            capabilityRPC = "CreateVolume"
	rpcControllerPublishVolume capabilityRPC = "ControllerPublishVolume"
	rpcValidateVolumeCaps      capabilityRPC = "ValidateVolumeCapabilities"
	rpcNodeStageVolume         capabilityRPC = "NodeStageVolume"
	rpcNodePublishVolume       capabilityRPC = "NodePublishVolume"
)

// capabilityRPCs lists every RPC that validates a volume capability. An accepted or rejected
// capability must reach the same verdict on all of them.
func capabilityRPCs() []capabilityRPC {
	return []capabilityRPC{
		rpcCreateVolume,
		rpcControllerPublishVolume,
		rpcValidateVolumeCaps,
		rpcNodeStageVolume,
		rpcNodePublishVolume,
	}
}

// capabilityProbeResult reports what one RPC attempt observed: the RPC error, whether
// ValidateVolumeCapabilities confirmed, the host operations the attempt reached, and the cloud
// mutations it left behind.
type capabilityProbeResult struct {
	err       error
	confirmed bool
	mounter   *recordingMounter
	created   int
	attached  int
}

// probeCapability runs one capability-validating RPC with cloud/host ops succeeding.
func probeCapability(
	t *testing.T,
	rpc capabilityRPC,
	volCap *csi.VolumeCapability,
	readonly bool,
) capabilityProbeResult {
	t.Helper()

	cfg := validTestConfig()
	ctx := context.Background()

	evsClient := newMockEVSClient()
	vol, err := evsClient.CreateVolume(ctx, evs.CreateVolumeOpts{
		Name:             "probe-vol",
		Size:             10,
		AvailabilityZone: cfg.AvailabilityZone,
		VolumeType:       "SSD",
	})
	if err != nil {
		t.Fatalf("failed to seed probe volume: %v", err)
	}

	controller, err := driver.NewControllerService(evsClient, cfg)
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	mounter := &recordingMounter{}
	node, err := driver.NewNodeService(mounter, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	tmpDir := t.TempDir()
	result := capabilityProbeResult{mounter: mounter}

	switch rpc {
	case rpcCreateVolume:
		_, result.err = controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
			Name:                      "probe-create",
			VolumeCapabilities:        []*csi.VolumeCapability{volCap},
			Parameters:                map[string]string{"type": "SSD"},
			AccessibilityRequirements: requisiteZone(cfg.AvailabilityZone),
		})

	case rpcControllerPublishVolume:
		_, result.err = controller.ControllerPublishVolume(
			ctx,
			&csi.ControllerPublishVolumeRequest{
				VolumeId:         vol.ID,
				NodeId:           cfg.NodeID,
				VolumeCapability: volCap,
				Readonly:         readonly,
			},
		)

	case rpcValidateVolumeCaps:
		var resp *csi.ValidateVolumeCapabilitiesResponse
		resp, result.err = controller.ValidateVolumeCapabilities(
			ctx,
			&csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           vol.ID,
				VolumeCapabilities: []*csi.VolumeCapability{volCap},
			},
		)
		result.confirmed = resp.GetConfirmed() != nil

	case rpcNodeStageVolume:
		_, result.err = node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
			VolumeId:          vol.ID,
			StagingTargetPath: filepath.Join(tmpDir, "staging"),
			PublishContext:    attachedPublishContext(),
			VolumeCapability:  volCap,
		})

	case rpcNodePublishVolume:
		_, result.err = node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId:          vol.ID,
			StagingTargetPath: filepath.Join(tmpDir, "staging"),
			TargetPath:        filepath.Join(tmpDir, "target"),
			PublishContext:    attachedPublishContext(),
			VolumeCapability:  volCap,
			Readonly:          readonly,
		})

	default:
		t.Fatalf("unknown capability RPC %q", rpc)
	}

	result.created = len(evsClient.volumes) - 1
	result.attached = len(evsClient.attachments)

	return result
}

// assertCapabilityVerdict checks InvalidArgument (or unconfirmed Validate).
func assertCapabilityVerdict(
	t *testing.T,
	rpc capabilityRPC,
	got capabilityProbeResult,
	accepted bool,
) {
	t.Helper()

	if accepted {
		if got.err != nil {
			t.Fatalf("%s: expected the capability to be accepted, got %v", rpc, got.err)
		}
		if rpc == rpcValidateVolumeCaps && !got.confirmed {
			t.Fatalf("%s: expected confirmation for an accepted capability", rpc)
		}

		return
	}

	if rpc == rpcValidateVolumeCaps {
		if got.err != nil {
			t.Fatalf("%s: expected an unconfirmed response, not a gRPC error: %v", rpc, got.err)
		}
		if got.confirmed {
			t.Fatalf("%s: expected no confirmation for a rejected capability", rpc)
		}

		return
	}

	if status.Code(got.err) != codes.InvalidArgument {
		t.Fatalf("%s: expected InvalidArgument, got %v", rpc, got.err)
	}

	switch rpc {
	case rpcCreateVolume:
		if got.created != 0 {
			t.Errorf("%s: expected no volume creation, got %d new volumes", rpc, got.created)
		}

	case rpcControllerPublishVolume:
		if got.attached != 0 {
			t.Errorf("%s: expected no attachment, got %d", rpc, got.attached)
		}

	case rpcNodeStageVolume, rpcNodePublishVolume:
		if got.mounter.discoverDevice != 0 {
			t.Errorf(
				"%s: expected no device discovery, got %d calls",
				rpc,
				got.mounter.discoverDevice,
			)
		}
		if got.mounter.formatAndMount != 0 {
			t.Errorf(
				"%s: expected no format or staging mount, got %d calls",
				rpc,
				got.mounter.formatAndMount,
			)
		}
		if len(got.mounter.mounts) != 0 {
			t.Errorf("%s: expected no mount, got %v", rpc, got.mounter.mounts)
		}
	}
}

func TestCapabilityContract_AcceptsOnlyTheTwoSupportedAccessModes(t *testing.T) {
	t.Parallel()

	for _, mode := range allAccessModes() {
		accepted := mode == csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER ||
			mode == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY
		for _, rpc := range capabilityRPCs() {
			t.Run(fmt.Sprintf("%s/%s", mode, rpc), func(t *testing.T) {
				t.Parallel()

				// NodePublishVolume is the only RPC that cross-checks the readonly flag against
				// the access mode.
				readonly := rpc == rpcNodePublishVolume &&
					mode == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY

				got := probeCapability(t, rpc, mountedCapability(mode, "ext4"), readonly)
				assertCapabilityVerdict(t, rpc, got, accepted)
			})
		}
	}
}

func TestCapabilityContract_AcceptsOnlyTheTwoSupportedFilesystems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fsType   string
		accepted bool
	}{
		{name: "ext4", fsType: "ext4", accepted: true},
		{name: "xfs", fsType: "xfs", accepted: true},
		{name: "omitted", fsType: "", accepted: true},
		{name: "arbitrary name", fsType: "btrfs", accepted: false},
	}

	for _, tt := range tests {
		for _, rpc := range capabilityRPCs() {
			t.Run(fmt.Sprintf("%s/%s", tt.name, rpc), func(t *testing.T) {
				t.Parallel()

				volCap := mountedCapability(
					csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					tt.fsType,
				)
				got := probeCapability(t, rpc, volCap, false)
				assertCapabilityVerdict(t, rpc, got, tt.accepted)
			})
		}
	}
}

func TestCapabilityContract_RawBlockConsultsNoFilesystem(t *testing.T) {
	t.Parallel()

	for _, rpc := range capabilityRPCs() {
		t.Run(string(rpc), func(t *testing.T) {
			t.Parallel()

			volCap := rawBlockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
			got := probeCapability(t, rpc, volCap, false)
			assertCapabilityVerdict(t, rpc, got, true)

			// Node stage/publish only: raw block must not format or set an fs type.
			if rpc != rpcNodeStageVolume && rpc != rpcNodePublishVolume {
				return
			}

			if got.mounter.formatAndMount != 0 {
				t.Errorf(
					"%s: expected raw block content never to be formatted, got %d calls",
					rpc,
					got.mounter.formatAndMount,
				)
			}
			for _, recorded := range got.mounter.mounts {
				if recorded.fsType != "" {
					t.Errorf(
						"%s: expected a raw block mount to name no filesystem, got %q",
						rpc,
						recorded.fsType,
					)
				}
			}
		})
	}
}

func TestControllerPublishVolume_RejectsAReadOnlyRequestWithoutAttaching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode csi.VolumeCapability_AccessMode_Mode
	}{
		{name: "writer mode", mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		{name: "reader only mode", mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			volCap := mountedCapability(tt.mode, "ext4")

			rejected := probeCapability(t, rpcControllerPublishVolume, volCap, true)
			if status.Code(rejected.err) != codes.InvalidArgument {
				t.Fatalf(
					"expected InvalidArgument for a read-only publish request, got %v",
					rejected.err,
				)
			}
			if rejected.attached != 0 {
				t.Errorf(
					"expected no attachment for a rejected read-only publish, got %d",
					rejected.attached,
				)
			}

			// Same request without the flag must attach.
			accepted := probeCapability(t, rpcControllerPublishVolume, volCap, false)
			if accepted.err != nil {
				t.Fatalf("expected a writable publish request to succeed, got %v", accepted.err)
			}
			if accepted.attached != 1 {
				t.Errorf("expected exactly one attachment, got %d", accepted.attached)
			}
		})
	}
}

func TestNodePublishVolume_RequiresAReadOnlyPublicationForAReadOnlyMode(t *testing.T) {
	t.Parallel()

	readerOnly := csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY
	writer := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER

	tests := []struct {
		name         string
		mode         csi.VolumeCapability_AccessMode_Mode
		readonly     bool
		accepted     bool
		wantReadOnly bool
	}{
		{
			name:     "reader only mode published writable",
			mode:     readerOnly,
			readonly: false,
			accepted: false,
		},
		{
			name:         "reader only mode published read-only",
			mode:         readerOnly,
			readonly:     true,
			accepted:     true,
			wantReadOnly: true,
		},
		{
			name:         "writer mode published read-only",
			mode:         writer,
			readonly:     true,
			accepted:     true,
			wantReadOnly: true,
		},
		{
			name:         "writer mode published writable",
			mode:         writer,
			readonly:     false,
			accepted:     true,
			wantReadOnly: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			volCap := mountedCapability(tt.mode, "ext4")
			got := probeCapability(t, rpcNodePublishVolume, volCap, tt.readonly)

			if !tt.accepted {
				if status.Code(got.err) != codes.InvalidArgument {
					t.Fatalf("expected InvalidArgument, got %v", got.err)
				}
				if len(got.mounter.mounts) != 0 {
					t.Errorf(
						"expected no mount for a rejected publication, got %v",
						got.mounter.mounts,
					)
				}

				return
			}

			if got.err != nil {
				t.Fatalf("expected the publication to succeed, got %v", got.err)
			}
			if len(got.mounter.mounts) != 1 {
				t.Fatalf("expected exactly one bind mount, got %d", len(got.mounter.mounts))
			}
			if hasOption(got.mounter.mounts[0].options, "ro") != tt.wantReadOnly {
				t.Errorf(
					"expected read-only mount option %v, got options %v",
					tt.wantReadOnly,
					got.mounter.mounts[0].options,
				)
			}
		})
	}
}

// conflictEVSClient returns ErrConflict from every EVS call.
type conflictEVSClient struct{}

func conflictError() error {
	return fmt.Errorf("volume state conflict: %w", evs.ErrConflict)
}

func (conflictEVSClient) CreateVolume(
	ctx context.Context,
	opts evs.CreateVolumeOpts,
) (*evs.Volume, error) {
	return nil, conflictError()
}

func (conflictEVSClient) GetVolume(ctx context.Context, id string) (*evs.Volume, error) {
	return nil, conflictError()
}

func (conflictEVSClient) DiscoverVolume(
	ctx context.Context,
	opts evs.DiscoverVolumeOpts,
) (*evs.Volume, error) {
	return nil, conflictError()
}

func (conflictEVSClient) DeleteVolume(ctx context.Context, id string) error {
	return conflictError()
}

func (conflictEVSClient) AttachVolume(
	ctx context.Context,
	volumeID, serverID string,
) (*evs.Attachment, error) {
	return nil, conflictError()
}

func (conflictEVSClient) DetachVolume(ctx context.Context, volumeID, serverID string) error {
	return conflictError()
}

// createConflictEVSClient: Discover not found, Create conflicts.
type createConflictEVSClient struct {
	conflictEVSClient
}

func (createConflictEVSClient) DiscoverVolume(
	ctx context.Context,
	opts evs.DiscoverVolumeOpts,
) (*evs.Volume, error) {
	return nil, fmt.Errorf("volume: %w", evs.ErrNotFound)
}

func TestControllerConflictCarriesTheStatusOfItsOperation(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	ctx := context.Background()

	svc, err := driver.NewControllerService(conflictEVSClient{}, cfg)
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	volCaps := []*csi.VolumeCapability{
		mountedCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "ext4"),
	}

	tests := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{
			name: "create volume",
			call: func() error {
				_, err := svc.CreateVolume(ctx, &csi.CreateVolumeRequest{
					Name:                      "conflict-vol",
					VolumeCapabilities:        volCaps,
					Parameters:                map[string]string{"type": "SSD"},
					AccessibilityRequirements: requisiteZone(cfg.AvailabilityZone),
				})

				return err
			},
			want: codes.AlreadyExists,
		},
		{
			name: "delete volume",
			call: func() error {
				_, err := svc.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "vol-1"})

				return err
			},
			want: codes.FailedPrecondition,
		},
		{
			name: "controller publish volume",
			call: func() error {
				_, err := svc.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
					VolumeId:         "vol-1",
					NodeId:           cfg.NodeID,
					VolumeCapability: volCaps[0],
				})

				return err
			},
			want: codes.FailedPrecondition,
		},
		{
			name: "controller unpublish volume",
			call: func() error {
				_, err := svc.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
					VolumeId: "vol-1",
					NodeId:   cfg.NodeID,
				})

				return err
			},
			want: codes.Aborted,
		},
		{
			name: "validate volume capabilities",
			call: func() error {
				_, err := svc.ValidateVolumeCapabilities(
					ctx,
					&csi.ValidateVolumeCapabilitiesRequest{
						VolumeId:           "vol-1",
						VolumeCapabilities: volCaps,
					},
				)

				return err
			},
			want: codes.Aborted,
		},
	}

	observed := make(map[codes.Code]struct{}, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := status.Code(tt.call())
			if got != tt.want {
				t.Errorf("expected %v for a cloud conflict, got %v", tt.want, got)
			}
			observed[got] = struct{}{}
		})
	}

	// The mapping is per-operation: one conflict must surface as several distinct codes.
	if len(observed) < 3 {
		t.Errorf(
			"expected one cloud conflict to carry several operation statuses, got %v",
			slices.Sorted(maps.Keys(observed)),
		)
	}

	// A conflict from CreateVolume itself (not discovery) must map the same way.
	createSvc, err := driver.NewControllerService(createConflictEVSClient{}, cfg)
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}
	_, err = createSvc.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:                      "conflict-vol",
		VolumeCapabilities:        volCaps,
		Parameters:                map[string]string{"type": "SSD"},
		AccessibilityRequirements: requisiteZone(cfg.AvailabilityZone),
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("expected AlreadyExists for a conflict raised by creation, got %v", err)
	}
}

func TestDeleteVolume_RejectsAVolumeWithoutTheOwnershipMarker(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	client.volumes["vol-unmarked"] = &evs.Volume{
		ID:               "vol-unmarked",
		Name:             "unmarked",
		Status:           "available",
		Size:             10,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	}

	svc, err := driver.NewControllerService(client, validTestConfig())
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	_, err = svc.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "vol-unmarked",
	})

	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for an unmarked volume, got %v: %v", code, err)
	}
}

func TestValidateVolumeCapabilities_EchoesEveryConfirmedField(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc, err := driver.NewControllerService(client, validTestConfig())
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	ctx := context.Background()
	vol, err := client.CreateVolume(ctx, evs.CreateVolumeOpts{
		Name:             "echo-vol",
		Size:             10,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})
	if err != nil {
		t.Fatalf("failed to seed volume: %v", err)
	}

	volCaps := []*csi.VolumeCapability{
		mountedCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "ext4"),
		mountedCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY, "xfs"),
		rawBlockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
	params := map[string]string{"type": "SSD", "availability_zone": "eu-de-01"}

	// Use a gRPC client so Confirmed is a decoded copy, not the request pointers.
	resp, err := newControllerClient(t, svc).ValidateVolumeCapabilities(
		ctx,
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           vol.ID,
			VolumeCapabilities: volCaps,
			Parameters:         params,
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	confirmed := resp.GetConfirmed()
	if confirmed == nil {
		t.Fatal("expected confirmation for a supported request")
	}

	if len(confirmed.GetVolumeCapabilities()) != len(volCaps) {
		t.Fatalf(
			"expected %d confirmed capabilities, got %d",
			len(volCaps),
			len(confirmed.GetVolumeCapabilities()),
		)
	}
	for i, want := range volCaps {
		if !proto.Equal(want, confirmed.GetVolumeCapabilities()[i]) {
			t.Errorf("confirmed capability %d does not echo the requested capability", i)
		}
	}

	if !maps.Equal(confirmed.GetParameters(), params) {
		t.Errorf(
			"expected the confirmed parameters to echo the request, got %v",
			confirmed.GetParameters(),
		)
	}

	// CreateVolume leaves volume context empty.
	if len(confirmed.GetVolumeContext()) != 0 {
		t.Errorf(
			"expected an empty confirmed volume context, got %v",
			confirmed.GetVolumeContext(),
		)
	}
}

func TestValidateVolumeCapabilities_UnconfirmedWithoutError(t *testing.T) {
	t.Parallel()

	client := newMockEVSClient()
	svc, err := driver.NewControllerService(client, validTestConfig())
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}

	ctx := context.Background()
	vol, err := client.CreateVolume(ctx, evs.CreateVolumeOpts{
		Name:             "unconfirmed-vol",
		Size:             10,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})
	if err != nil {
		t.Fatalf("failed to seed volume: %v", err)
	}

	tests := []struct {
		name          string
		volumeContext map[string]string
		parameters    map[string]string
		confirm       bool
	}{
		{
			name:          "volume context this driver never issued",
			volumeContext: map[string]string{"fsType": "ext4"},
			parameters:    map[string]string{"type": "SSD"},
			confirm:       false,
		},
		{
			name:       "unsupported parameter key",
			parameters: map[string]string{"type": "SSD", "encrypted": "true"},
			confirm:    false,
		},
		{
			name:       "parameter key reserved for the orchestrator",
			parameters: map[string]string{"type": "SSD", "csi.storage.k8s.io/pvc/name": "data"},
			confirm:    true,
		},
		{
			name:       "no volume context and only accepted parameters",
			parameters: map[string]string{"type": "SSD", "availability_zone": "eu-de-01"},
			confirm:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := svc.ValidateVolumeCapabilities(
				ctx,
				&csi.ValidateVolumeCapabilitiesRequest{
					VolumeId: vol.ID,
					VolumeCapabilities: []*csi.VolumeCapability{
						mountedCapability(
							csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
							"ext4",
						),
					},
					VolumeContext: tt.volumeContext,
					Parameters:    tt.parameters,
				},
			)
			// Unsupported fields return Confirmed=nil without a gRPC error.
			if err != nil {
				t.Fatalf("expected Confirmed=nil, got error: %v", err)
			}
			if (resp.GetConfirmed() != nil) != tt.confirm {
				t.Errorf(
					"expected confirmed=%v, got confirmed=%v with message %q",
					tt.confirm,
					resp.GetConfirmed() != nil,
					resp.GetMessage(),
				)
			}
		})
	}
}

func TestNodeCapabilitySetCoversEveryAcceptedAccessMode(t *testing.T) {
	t.Parallel()

	mounter := &recordingMounter{}
	svc, err := driver.NewNodeService(mounter, validTestConfig())
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	resp, err := newNodeClient(t, svc).NodeGetCapabilities(
		context.Background(),
		&csi.NodeGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("NodeGetCapabilities failed: %v", err)
	}

	advertised := make(map[csi.NodeServiceCapability_RPC_Type]struct{}, len(resp.GetCapabilities()))
	for _, capability := range resp.GetCapabilities() {
		advertised[capability.GetRpc().GetType()] = struct{}{}
	}

	// The CSI spec gates these modes behind a Node capability this driver doesn't advertise.
	gatedBy := map[csi.VolumeCapability_AccessMode_Mode]csi.NodeServiceCapability_RPC_Type{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER: csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:  csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}

	// Both accepted modes require staging; fail fast if it isn't advertised.
	if _, ok := advertised[csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME]; !ok {
		t.Errorf(
			"expected the Node service to advertise STAGE_UNSTAGE_VOLUME, got %v",
			resp.GetCapabilities(),
		)
	}

	accepted := make([]csi.VolumeCapability_AccessMode_Mode, 0, 2)
	for _, mode := range allAccessModes() {
		got := probeCapability(t, rpcNodeStageVolume, mountedCapability(mode, "ext4"), false)
		if got.err != nil {
			continue
		}
		accepted = append(accepted, mode)

		gate, gated := gatedBy[mode]
		if !gated {
			continue
		}
		if _, ok := advertised[gate]; !ok {
			t.Errorf(
				"access mode %s is accepted but its required Node capability %s is not advertised",
				mode,
				gate,
			)
		}
	}

	want := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
	}
	slices.Sort(want)
	if !slices.Equal(accepted, want) {
		t.Errorf(
			"expected exactly the two ungated single-node modes to be accepted, got %v",
			accepted,
		)
	}
}
