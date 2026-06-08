package driver_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mountutils "k8s.io/mount-utils"
	k8sexec "k8s.io/utils/exec"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"wilaris.dev/t-cloud-public-csi-driver/internal/mount"
)

// emulatedVolumeID is the volume the emulated host has an attached device for.
const emulatedVolumeID = "vol-6f1b2c3d"

// hostCmd answers one block-device command from a canned handler.
type hostCmd struct {
	args    []string
	handler func(args ...string) ([]byte, error)
}

func (c *hostCmd) CombinedOutput() ([]byte, error) {
	if c.handler != nil {
		return c.handler(c.args...)
	}
	return nil, nil
}

func (c *hostCmd) Output() ([]byte, error)            { return c.CombinedOutput() }
func (c *hostCmd) Run() error                         { _, err := c.CombinedOutput(); return err }
func (c *hostCmd) Start() error                       { return nil }
func (c *hostCmd) Wait() error                        { return nil }
func (c *hostCmd) Stop()                              {}
func (c *hostCmd) SetDir(_ string)                    {}
func (c *hostCmd) SetStdin(_ io.Reader)               {}
func (c *hostCmd) SetStdout(_ io.Writer)              {}
func (c *hostCmd) SetStderr(_ io.Writer)              {}
func (c *hostCmd) SetEnv(_ []string)                  {}
func (c *hostCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (c *hostCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }

// hostExec records and answers the host commands the mount package runs.
type hostExec struct {
	handlers map[string]func(args ...string) ([]byte, error)
	calls    []string
}

func (e *hostExec) Command(cmd string, args ...string) k8sexec.Cmd {
	e.calls = append(e.calls, strings.TrimSpace(cmd+" "+strings.Join(args, " ")))
	return &hostCmd{args: args, handler: e.handlers[cmd]}
}

func (e *hostExec) CommandContext(
	_ context.Context,
	cmd string,
	args ...string,
) k8sexec.Cmd {
	return e.Command(cmd, args...)
}

func (e *hostExec) LookPath(file string) (string, error) { return file, nil }

// countCalls returns how many recorded commands start with prefix.
func (e *hostExec) countCalls(prefix string) int {
	count := 0
	for _, call := range e.calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

// emulatedHost stands in for a Linux worker node offline: an in-memory kernel mount table,
// a `/dev/disk/by-id` directory holding the device of an attached EVS volume, and canned
// `blkid`/`mkfs` binaries.
type emulatedHost struct {
	mounter    *mount.LinuxMounter
	table      *mountutils.FakeMounter
	exec       *hostExec
	devicePath string
	dir        string
}

func newEmulatedHost(t *testing.T) *emulatedHost {
	t.Helper()

	// Paths are resolved up front because the kernel mount table records resolved paths.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to resolve temporary directory: %v", err)
	}

	byIDDir := filepath.Join(dir, "by-id")
	if err := os.MkdirAll(byIDDir, 0o750); err != nil {
		t.Fatalf("failed to create by-id directory: %v", err)
	}

	// udev exposes an attached VirtIO volume under its truncated serial.
	devicePath := filepath.Join(byIDDir, "virtio-"+emulatedVolumeID)
	if err := os.WriteFile(devicePath, []byte("attached-evs-volume"), 0o600); err != nil {
		t.Fatalf("failed to create attached device: %v", err)
	}

	hostCommands := &hostExec{
		handlers: map[string]func(args ...string) ([]byte, error){
			// A freshly attached EVS volume carries no filesystem yet.
			"blkid":     func(_ ...string) ([]byte, error) { return []byte(""), nil },
			"lsblk":     func(_ ...string) ([]byte, error) { return []byte(""), nil },
			"mkfs.ext4": func(_ ...string) ([]byte, error) { return []byte("done"), nil },
			"mkfs.xfs":  func(_ ...string) ([]byte, error) { return []byte("done"), nil },
		},
	}

	table := mountutils.NewFakeMounter(nil)

	return &emulatedHost{
		mounter: mount.NewLinuxMounter(
			mount.WithMountUtilsInterface(table),
			mount.WithExecInterface(hostCommands),
			mount.WithDiskByIDDir(byIDDir),
		),
		table:      table,
		exec:       hostCommands,
		devicePath: devicePath,
		dir:        dir,
	}
}

// mountPointAt returns the mount table entry for path, if the path is mounted.
func (h *emulatedHost) mountPointAt(path string) (mountutils.MountPoint, bool) {
	for _, mp := range h.table.MountPoints {
		if mp.Path == path {
			return mp, true
		}
	}
	return mountutils.MountPoint{}, false
}

// newEmulatedNodeClient serves a Node service backed by host over an in-process connection.
func newEmulatedNodeClient(t *testing.T, host *emulatedHost) csi.NodeClient {
	t.Helper()

	svc, err := driver.NewNodeService(host.mounter, validTestConfig())
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	return newNodeClient(t, svc)
}

func TestNodeService_MountedVolumeLifecycleOnEmulatedHost(t *testing.T) {
	t.Parallel()

	host := newEmulatedHost(t)
	client := newEmulatedNodeClient(t, host)
	ctx := context.Background()

	stagingPath := filepath.Join(host.dir, "staging")
	targetPath := filepath.Join(host.dir, "target")

	stage := &csi.NodeStageVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCapability(""),
	}
	publish := &csi.NodePublishVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability(""),
	}

	if _, err := client.NodeStageVolume(ctx, stage); err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}

	staged, ok := host.mountPointAt(stagingPath)
	if !ok {
		t.Fatalf(
			"expected the attached device staged at %s, mount table: %+v",
			stagingPath,
			host.table.MountPoints,
		)
	}
	if staged.Device != host.devicePath {
		t.Errorf("expected staged device %s, got %s", host.devicePath, staged.Device)
	}
	// A request without a filesystem type must still produce a usable filesystem.
	if staged.Type != mount.DefaultFilesystemType {
		t.Errorf("expected staged filesystem %s, got %q", mount.DefaultFilesystemType, staged.Type)
	}
	if host.exec.countCalls("mkfs.ext4") != 1 {
		t.Errorf("expected the device to be formatted once, got calls %v", host.exec.calls)
	}

	if _, err := client.NodePublishVolume(ctx, publish); err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}

	published, ok := host.mountPointAt(targetPath)
	if !ok {
		t.Fatalf(
			"expected the volume published at %s, mount table: %+v",
			targetPath,
			host.table.MountPoints,
		)
	}
	// A bind mount of the staged directory resolves to the same device, so the workload
	// reaches the EVS volume itself at its target path.
	if published.Device != host.devicePath {
		t.Errorf("expected published device %s, got %s", host.devicePath, published.Device)
	}
	if !hasOption(published.Opts, "bind") {
		t.Errorf("expected a bind mount at the target, got options %v", published.Opts)
	}

	accessible, err := host.mounter.IsMountPoint(ctx, targetPath)
	if err != nil || !accessible {
		t.Errorf("published volume is not accessible at %s (err=%v)", targetPath, err)
	}

	// Repeating the sequence must converge on the same host state rather than stacking
	// mounts or reformatting a device that already carries data.
	for range 2 {
		if _, err := client.NodeStageVolume(ctx, stage); err != nil {
			t.Fatalf("repeated NodeStageVolume failed: %v", err)
		}
		if _, err := client.NodePublishVolume(ctx, publish); err != nil {
			t.Fatalf("repeated NodePublishVolume failed: %v", err)
		}
	}
	if len(host.table.MountPoints) != 2 {
		t.Errorf(
			"expected only the staging and target mounts, got %+v",
			host.table.MountPoints,
		)
	}
	if host.exec.countCalls("mkfs.") != 1 {
		t.Errorf("expected exactly one format for the volume, got calls %v", host.exec.calls)
	}

	unpublish := &csi.NodeUnpublishVolumeRequest{
		VolumeId:   emulatedVolumeID,
		TargetPath: targetPath,
	}
	unstage := &csi.NodeUnstageVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: stagingPath,
	}

	if _, err := client.NodeUnpublishVolume(ctx, unpublish); err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	if _, ok := host.mountPointAt(targetPath); ok {
		t.Errorf("unpublish left a mount at %s: %+v", targetPath, host.table.MountPoints)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Errorf("unpublish left the target path %s behind (err=%v)", targetPath, err)
	}

	if _, err := client.NodeUnstageVolume(ctx, unstage); err != nil {
		t.Fatalf("NodeUnstageVolume failed: %v", err)
	}
	if len(host.table.MountPoints) != 0 {
		t.Errorf("teardown leaked mounts: %+v", host.table.MountPoints)
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Errorf("unstage left the staging path %s behind (err=%v)", stagingPath, err)
	}
	// Removing the attached device is the controller's detach, not the node's teardown.
	if _, err := os.Stat(host.devicePath); err != nil {
		t.Errorf("node teardown removed the attached device: %v", err)
	}

	// Teardown of an already torn down volume must stay successful and stay clean.
	if _, err := client.NodeUnpublishVolume(ctx, unpublish); err != nil {
		t.Errorf("repeated NodeUnpublishVolume failed: %v", err)
	}
	if _, err := client.NodeUnstageVolume(ctx, unstage); err != nil {
		t.Errorf("repeated NodeUnstageVolume failed: %v", err)
	}
	if len(host.table.MountPoints) != 0 {
		t.Errorf("repeated teardown leaked mounts: %+v", host.table.MountPoints)
	}
}

func TestNodeService_RawBlockVolumeLifecycleOnEmulatedHost(t *testing.T) {
	t.Parallel()

	host := newEmulatedHost(t)
	client := newEmulatedNodeClient(t, host)
	ctx := context.Background()

	stagingPath := filepath.Join(host.dir, "block-staging")
	targetPath := filepath.Join(host.dir, "block-target", "device")

	stage := &csi.NodeStageVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  blockCapability(),
	}
	publish := &csi.NodePublishVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  blockCapability(),
	}

	if _, err := client.NodeStageVolume(ctx, stage); err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}
	if _, err := os.Stat(stagingPath); err != nil {
		t.Errorf("expected the staging path %s to exist: %v", stagingPath, err)
	}
	if len(host.table.MountPoints) != 0 {
		t.Errorf("staging a raw block volume must not mount it, got %+v", host.table.MountPoints)
	}

	if _, err := client.NodePublishVolume(ctx, publish); err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}

	published, ok := host.mountPointAt(targetPath)
	if !ok {
		t.Fatalf(
			"expected the raw device published at %s, mount table: %+v",
			targetPath,
			host.table.MountPoints,
		)
	}
	if published.Device != host.devicePath {
		t.Errorf("expected published device %s, got %s", host.devicePath, published.Device)
	}
	if published.Type != "" {
		t.Errorf("expected no filesystem type for a raw block mount, got %q", published.Type)
	}
	if !hasOption(published.Opts, "bind") {
		t.Errorf("expected a bind mount at the target, got options %v", published.Opts)
	}
	if info, err := os.Stat(targetPath); err != nil || info.IsDir() {
		t.Errorf("expected a file at the raw block target %s (err=%v)", targetPath, err)
	}
	// A raw block volume is handed to the workload exactly as it is.
	if host.exec.countCalls("mkfs.") != 0 {
		t.Errorf("a raw block volume must never be formatted, got calls %v", host.exec.calls)
	}

	// Republishing must not stack a second mount over the device.
	for range 2 {
		if _, err := client.NodePublishVolume(ctx, publish); err != nil {
			t.Fatalf("repeated NodePublishVolume failed: %v", err)
		}
	}
	if len(host.table.MountPoints) != 1 {
		t.Errorf("expected only the target mount, got %+v", host.table.MountPoints)
	}

	if _, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   emulatedVolumeID,
		TargetPath: targetPath,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	if len(host.table.MountPoints) != 0 {
		t.Errorf("unpublish leaked mounts: %+v", host.table.MountPoints)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Errorf("unpublish left the target path %s behind (err=%v)", targetPath, err)
	}

	if _, err := client.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: stagingPath,
	}); err != nil {
		t.Fatalf("NodeUnstageVolume failed: %v", err)
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Errorf("unstage left the staging path %s behind (err=%v)", stagingPath, err)
	}
	if _, err := os.Stat(host.devicePath); err != nil {
		t.Errorf("node teardown removed the attached device: %v", err)
	}
}

func TestNodeService_PublishFailureLeavesNoTargetOnEmulatedHost(t *testing.T) {
	t.Parallel()

	host := newEmulatedHost(t)
	client := newEmulatedNodeClient(t, host)
	ctx := context.Background()

	unstagedTarget := filepath.Join(host.dir, "unstaged-target")
	_, err := client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: filepath.Join(host.dir, "never-staged"),
		TargetPath:        unstagedTarget,
		VolumeCapability:  mountCapability("ext4"),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition for an unstaged volume, got %v", err)
	}
	if _, statErr := os.Stat(unstagedTarget); !os.IsNotExist(statErr) {
		t.Errorf("failed publish created the target %s (err=%v)", unstagedTarget, statErr)
	}

	// A volume with no device attached to this node cannot be published as raw block.
	detachedTarget := filepath.Join(host.dir, "detached-target", "device")
	_, err = client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-not-attached",
		StagingTargetPath: filepath.Join(host.dir, "block-staging"),
		TargetPath:        detachedTarget,
		VolumeCapability:  blockCapability(),
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for an unattached volume, got %v", err)
	}
	if _, statErr := os.Stat(detachedTarget); !os.IsNotExist(statErr) {
		t.Errorf("failed publish created the target %s (err=%v)", detachedTarget, statErr)
	}

	if len(host.table.MountPoints) != 0 {
		t.Errorf("a failed publish mounted something: %+v", host.table.MountPoints)
	}
}

func TestNodeService_UnpublishReportsRemainingMountOnEmulatedHost(t *testing.T) {
	t.Parallel()

	host := newEmulatedHost(t)
	client := newEmulatedNodeClient(t, host)
	ctx := context.Background()

	stagingPath := filepath.Join(host.dir, "staging")
	targetPath := filepath.Join(host.dir, "target")

	if _, err := client.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCapability("ext4"),
	}); err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}
	if _, err := client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          emulatedVolumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	}); err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}

	// A target that the host refuses to unmount must not be reported as unpublished.
	host.table.UnmountFunc = func(_ string) error {
		return errors.New("device or resource busy")
	}

	_, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   emulatedVolumeID,
		TargetPath: targetPath,
	})
	if status.Code(err) != codes.Internal {
		t.Errorf("expected Internal when the target cannot be unmounted, got %v", err)
	}
	if _, ok := host.mountPointAt(targetPath); !ok {
		t.Errorf("expected the mount at %s to remain, got %+v", targetPath, host.table.MountPoints)
	}
	if _, statErr := os.Stat(targetPath); statErr != nil {
		t.Errorf("expected the still mounted target %s to remain: %v", targetPath, statErr)
	}
}

func TestNodeService_NodeGetInfoReportsNodeIDFromFlags(t *testing.T) {
	t.Parallel()

	const serverUUID = "9f0a1b2c-3d4e-5f60-7182-93a4b5c6d7e8"
	const zone = "eu-de-01"

	env := map[string]string{
		config.EnvAuthURL:    "https://iam.example.com/v3",
		config.EnvAccessKey:  "test-ak",
		config.EnvSecretKey:  "test-sk",
		config.EnvProjectID:  "test-project-id",
		config.EnvRegionName: "eu-de",
	}

	cfg, err := config.Parse(
		[]string{"--nodeid", serverUUID, "--availability-zone", zone},
		func(key string) string { return env[key] },
	)
	if err != nil {
		t.Fatalf("config.Parse failed: %v", err)
	}

	svc, err := driver.NewNodeService(&fakeMounter{}, cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	client := newNodeClient(t, svc)

	res, err := client.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo failed: %v", err)
	}

	// The compute instance Server UUID the operator passes on the command line is what
	// the controller later attaches EVS volumes to, so it must be reported unchanged.
	if res.GetNodeId() != serverUUID {
		t.Errorf("expected node_id %q from --nodeid, got %q", serverUUID, res.GetNodeId())
	}

	gotZone := res.GetAccessibleTopology().GetSegments()[driver.TopologyZoneKey]
	if gotZone != zone {
		t.Errorf("expected zone topology %q, got %q", zone, gotZone)
	}
	if gotZone == env[config.EnvRegionName] {
		t.Errorf("node reported region %q as its zone topology", gotZone)
	}
}
