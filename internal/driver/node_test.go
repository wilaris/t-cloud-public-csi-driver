package driver_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"wilaris.dev/t-cloud-public-csi-driver/internal/mount"
)

type fakeMounter struct {
	discoverDeviceFn    func(ctx context.Context, id string) (string, error)
	formatAndMountFn    func(ctx context.Context, source, target, fsType string, options []string) error
	mountFn             func(ctx context.Context, source, target, fsType string, options []string) error
	unmountFn           func(ctx context.Context, target string) error
	isMountPointFn      func(ctx context.Context, target string) (bool, error)
	getFilesystemTypeFn func(ctx context.Context, source string) (string, error)
	getMountSourceFn    func(ctx context.Context, target string) (string, error)
}

func (f *fakeMounter) DiscoverDevice(ctx context.Context, id string) (string, error) {
	if f.discoverDeviceFn != nil {
		return f.discoverDeviceFn(ctx, id)
	}
	return "/dev/sdb", nil
}

func (f *fakeMounter) FormatAndMount(
	ctx context.Context,
	source, target, fsType string,
	options []string,
) error {
	if f.formatAndMountFn != nil {
		return f.formatAndMountFn(ctx, source, target, fsType, options)
	}
	return nil
}

func (f *fakeMounter) Mount(
	ctx context.Context,
	source, target, fsType string,
	options []string,
) error {
	if f.mountFn != nil {
		return f.mountFn(ctx, source, target, fsType, options)
	}
	return nil
}

func (f *fakeMounter) Unmount(ctx context.Context, target string) error {
	if f.unmountFn != nil {
		return f.unmountFn(ctx, target)
	}
	return nil
}

func (f *fakeMounter) IsMountPoint(ctx context.Context, target string) (bool, error) {
	if f.isMountPointFn != nil {
		return f.isMountPointFn(ctx, target)
	}
	return false, nil
}

func (f *fakeMounter) GetFilesystemType(ctx context.Context, source string) (string, error) {
	if f.getFilesystemTypeFn != nil {
		return f.getFilesystemTypeFn(ctx, source)
	}
	return "ext4", nil
}

func (f *fakeMounter) GetMountSource(ctx context.Context, target string) (string, error) {
	if f.getMountSourceFn != nil {
		return f.getMountSourceFn(ctx, target)
	}
	return "/dev/sdb", nil
}

func TestNewNodeService_Validation(t *testing.T) {
	t.Parallel()

	fm := &fakeMounter{}
	cfg := validTestConfig()

	tests := []struct {
		name    string
		mounter mount.Mounter
		cfg     *config.Config
		wantErr bool
	}{
		{
			name:    "nil mounter",
			mounter: nil,
			cfg:     cfg,
			wantErr: true,
		},
		{
			name:    "nil config",
			mounter: fm,
			cfg:     nil,
			wantErr: true,
		},
		{
			name:    "empty node ID",
			mounter: fm,
			cfg: &config.Config{
				NodeID: "",
			},
			wantErr: true,
		},
		{
			name:    "empty availability zone",
			mounter: fm,
			cfg: &config.Config{
				NodeID:           "12345678-1234-1234-1234-123456789012",
				AvailabilityZone: "",
			},
			wantErr: true,
		},
		{
			name:    "valid inputs",
			mounter: fm,
			cfg:     cfg,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, err := driver.NewNodeService(tt.mounter, tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewNodeService() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && svc == nil {
				t.Fatal("expected non-nil NodeService")
			}
		})
	}
}

func TestNodeService_NodeGetInfo(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	fm := &fakeMounter{}
	svc, err := driver.NewNodeService(fm, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)

	res, err := client.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo failed: %v", err)
	}

	if res.GetNodeId() != cfg.NodeID {
		t.Errorf("expected NodeId %q, got %q", cfg.NodeID, res.GetNodeId())
	}

	topo := res.GetAccessibleTopology()
	if topo == nil {
		t.Fatal("expected accessible topology")
	}

	// The node must report its availability zone, not its region: a region spans several
	// zones, so it can never match the zone CreateVolume reports for a provisioned volume.
	gotZone := topo.GetSegments()[driver.TopologyZoneKey]
	if gotZone != cfg.AvailabilityZone {
		t.Errorf("expected zone topology %q, got %q", cfg.AvailabilityZone, gotZone)
	}
	if gotZone == cfg.RegionName {
		t.Errorf("node reported region %q as its zone topology", cfg.RegionName)
	}
}

func TestNodeService_NodeGetCapabilities(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	fm := &fakeMounter{}
	svc, err := driver.NewNodeService(fm, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)

	res, err := client.NodeGetCapabilities(
		context.Background(),
		&csi.NodeGetCapabilitiesRequest{},
	)
	if err != nil {
		t.Fatalf("NodeGetCapabilities failed: %v", err)
	}

	caps := res.GetCapabilities()
	if len(caps) != 1 {
		t.Fatalf("expected 1 capability, got %d", len(caps))
	}

	rpcCap := caps[0].GetRpc()
	if rpcCap == nil || rpcCap.GetType() != csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME {
		t.Errorf("expected STAGE_UNSTAGE_VOLUME capability, got %v", rpcCap)
	}
}

func TestNodeService_NodeStageVolume(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	tmpDir := t.TempDir()
	stagingPath := filepath.Join(tmpDir, "staging")

	var formattedAndMounted bool
	fm := &fakeMounter{
		discoverDeviceFn: func(ctx context.Context, id string) (string, error) {
			if id == "vol-missing" {
				return "", mount.ErrDeviceNotFound
			}
			return "/dev/sdb", nil
		},
		formatAndMountFn: func(ctx context.Context, source, target, fsType string, options []string) error {
			formattedAndMounted = true
			return nil
		},
	}

	svc, err := driver.NewNodeService(fm, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)

	// 1. Missing volume ID
	_, err = client.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCapability(""),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for missing volume_id, got: %v", err)
	}

	// 2. Device not found
	_, err = client.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-missing",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCapability(""),
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for missing device, got: %v", err)
	}

	// 3. Successful Mount Access Stage
	_, err = client.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume mount mode failed: %v", err)
	}
	if !formattedAndMounted {
		t.Error("expected FormatAndMount to be called for Mount access type")
	}

	// 4. Successful Block Access Stage
	blockStagingPath := filepath.Join(tmpDir, "block-staging")
	_, err = client.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: blockStagingPath,
		VolumeCapability:  blockCapability(),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume block mode failed: %v", err)
	}
	if _, err := os.Stat(blockStagingPath); os.IsNotExist(err) {
		t.Errorf("expected block staging directory %s to be created", blockStagingPath)
	}
}

func TestNodeService_NodeUnstageVolume(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	tmpDir := t.TempDir()
	stagingPath := filepath.Join(tmpDir, "staging")
	_ = os.MkdirAll(stagingPath, 0o750)

	var unmounted bool
	fm := &fakeMounter{
		unmountFn: func(ctx context.Context, target string) error {
			unmounted = true
			return nil
		},
	}

	svc, err := driver.NewNodeService(fm, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)

	// Missing parameters
	_, err = client.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty req, got: %v", err)
	}

	// Success unstage
	_, err = client.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume failed: %v", err)
	}
	if !unmounted {
		t.Error("expected Unmount to be called")
	}
}

// recordedMount captures the arguments of a Mounter.Mount call.
type recordedMount struct {
	source  string
	target  string
	fsType  string
	options []string
}

func hasOption(options []string, want string) bool {
	return slices.Contains(options, want)
}

func TestNodeService_NodePublishVolume(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	tmpDir := t.TempDir()
	stagingPath := filepath.Join(tmpDir, "staging")
	targetPath := filepath.Join(tmpDir, "target")

	var mounts []recordedMount
	var formatted bool
	fm := &fakeMounter{
		discoverDeviceFn: func(ctx context.Context, id string) (string, error) {
			return "/dev/sdb", nil
		},
		formatAndMountFn: func(ctx context.Context, source, target, fsType string, options []string) error {
			formatted = true
			return nil
		},
		mountFn: func(ctx context.Context, source, target, fsType string, options []string) error {
			mounts = append(mounts, recordedMount{
				source:  source,
				target:  target,
				fsType:  fsType,
				options: options,
			})
			return nil
		},
		isMountPointFn: func(ctx context.Context, target string) (bool, error) {
			return target == stagingPath, nil
		},
	}

	svc, err := driver.NewNodeService(fm, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)

	// 1. Invalid access mode
	_, err = client.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for multi-node access mode, got: %v", err)
	}

	// 2. Successful Mount Publish bind mounts the staged directory without formatting it.
	_, err = client.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume mount mode failed: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 bind mount for publish, got %d", len(mounts))
	}
	if mounts[0].source != stagingPath || mounts[0].target != targetPath {
		t.Errorf(
			"expected bind mount %s -> %s, got %s -> %s",
			stagingPath,
			targetPath,
			mounts[0].source,
			mounts[0].target,
		)
	}
	if !hasOption(mounts[0].options, "bind") {
		t.Errorf("expected bind option for publish, got %v", mounts[0].options)
	}
	if mounts[0].fsType != "" {
		t.Errorf("expected empty fsType for a bind mount, got %q", mounts[0].fsType)
	}

	// 3. A read-only request adds the ro option.
	roTargetPath := filepath.Join(tmpDir, "target-ro")
	mounts = nil
	_, err = client.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: stagingPath,
		TargetPath:        roTargetPath,
		Readonly:          true,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			},
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume read-only mount failed: %v", err)
	}
	if len(mounts) != 1 || !hasOption(mounts[0].options, "ro") {
		t.Errorf("expected ro option for read-only publish, got %v", mounts)
	}

	// 4. Publishing a volume that was never staged is a precondition failure.
	mounts = nil
	_, err = client.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: filepath.Join(tmpDir, "unstaged"),
		TargetPath:        filepath.Join(tmpDir, "target-unstaged"),
		VolumeCapability:  mountCapability(""),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition for unstaged volume, got: %v", err)
	}
	if len(mounts) != 0 {
		t.Errorf("expected no mount for an unstaged volume, got %v", mounts)
	}

	// 5. Successful Block Publish bind mounts the device itself.
	blockTargetPath := filepath.Join(tmpDir, "block-target", "file.raw")
	mounts = nil
	_, err = client.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: stagingPath,
		TargetPath:        blockTargetPath,
		VolumeCapability:  blockCapability(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume block mode failed: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 bind mount for raw block publish, got %d", len(mounts))
	}
	if mounts[0].source != "/dev/sdb" || mounts[0].target != blockTargetPath {
		t.Errorf(
			"expected bind mount /dev/sdb -> %s, got %s -> %s",
			blockTargetPath,
			mounts[0].source,
			mounts[0].target,
		)
	}
	if mounts[0].fsType != "" {
		t.Errorf("expected empty fsType for raw block publish, got %q", mounts[0].fsType)
	}
	if _, err := os.Stat(blockTargetPath); os.IsNotExist(err) {
		t.Errorf("expected target file %s to be created for raw block publish", blockTargetPath)
	}

	// A raw block device and a staged directory must never be formatted at publish time.
	if formatted {
		t.Error("expected NodePublishVolume to bind mount only, but FormatAndMount was called")
	}
}

func TestNodeService_NodeUnpublishVolume(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	_ = os.MkdirAll(targetPath, 0o750)

	var unpublished bool
	fm := &fakeMounter{
		unmountFn: func(ctx context.Context, target string) error {
			unpublished = true
			return nil
		},
	}

	svc, err := driver.NewNodeService(fm, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)

	// Invalid input
	_, err = client.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty req, got: %v", err)
	}

	// Successful unpublish
	_, err = client.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "vol-123",
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	if !unpublished {
		t.Error("expected Unmount to be called")
	}
}

func TestNodeService_ErrorHandling(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	tmpDir := t.TempDir()

	fm := &fakeMounter{
		formatAndMountFn: func(ctx context.Context, source, target, fsType string, options []string) error {
			return errors.New("mount system error")
		},
		mountFn: func(ctx context.Context, source, target, fsType string, options []string) error {
			return errors.New("bind mount system error")
		},
		unmountFn: func(ctx context.Context, target string) error {
			return errors.New("unmount error")
		},
		isMountPointFn: func(ctx context.Context, target string) (bool, error) {
			return true, nil
		},
	}

	svc, err := driver.NewNodeService(fm, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)

	// Format and mount error returns Internal code
	_, err = client.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: filepath.Join(tmpDir, "stage-err"),
		VolumeCapability:  mountCapability(""),
	})
	if status.Code(err) != codes.Internal {
		t.Errorf("expected Internal status code for stage error, got: %v", status.Code(err))
	}

	// Bind mount error returns Internal code
	_, err = client.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: filepath.Join(tmpDir, "stage-ok"),
		TargetPath:        filepath.Join(tmpDir, "publish-err"),
		VolumeCapability:  mountCapability(""),
	})
	if status.Code(err) != codes.Internal {
		t.Errorf("expected Internal status code for publish error, got: %v", status.Code(err))
	}

	// Unmount error returns Internal code
	_, err = client.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "vol-123",
		StagingTargetPath: filepath.Join(tmpDir, "unstage-err"),
	})
	if status.Code(err) != codes.Internal {
		t.Errorf("expected Internal status code for unstage error, got: %v", status.Code(err))
	}
}

func TestNodeService_DeviceDiscoveryFromPublishContext(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig()
	tmpDir := t.TempDir()

	tests := []struct {
		name           string
		publishContext map[string]string
		known          string
		wantIdentifier string
	}{
		{
			name:           "prefers the device path reported at attach time",
			publishContext: map[string]string{"devicePath": "/dev/vdb"},
			known:          "/dev/vdb",
			wantIdentifier: "/dev/vdb",
		},
		{
			name:           "falls back to the volume ID when no device path is published",
			publishContext: nil,
			known:          "vol-123",
			wantIdentifier: "vol-123",
		},
		{
			name:           "falls back to the volume ID when the published path is stale",
			publishContext: map[string]string{"devicePath": "/dev/gone"},
			known:          "vol-123",
			wantIdentifier: "vol-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var resolved string
			fm := &fakeMounter{
				discoverDeviceFn: func(ctx context.Context, id string) (string, error) {
					if id != tt.known {
						return "", mount.ErrDeviceNotFound
					}
					resolved = id
					return "/dev/sdb", nil
				},
			}

			svc, err := driver.NewNodeService(fm, cfg)
			if err != nil {
				t.Fatalf("NewNodeService failed: %v", err)
			}

			client := newNodeClient(t, svc)

			_, err = client.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          "vol-123",
				StagingTargetPath: filepath.Join(tmpDir, "staging"),
				PublishContext:    tt.publishContext,
				VolumeCapability:  mountCapability(""),
			})
			if err != nil {
				t.Fatalf("NodeStageVolume failed: %v", err)
			}
			if resolved != tt.wantIdentifier {
				t.Errorf(
					"expected device discovery via %q, got %q",
					tt.wantIdentifier,
					resolved,
				)
			}
		})
	}
}
