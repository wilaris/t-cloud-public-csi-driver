package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	mountutils "k8s.io/mount-utils"
	k8sexec "k8s.io/utils/exec"
)

type fakeExec struct {
	handlers map[string]func(args ...string) ([]byte, error)
	calls    []string
}

type fakeCmd struct {
	cmd     string
	args    []string
	handler func(args ...string) ([]byte, error)
}

func (f *fakeCmd) CombinedOutput() ([]byte, error) {
	if f.handler != nil {
		return f.handler(f.args...)
	}
	return nil, nil
}
func (f *fakeCmd) Output() ([]byte, error)            { return f.CombinedOutput() }
func (f *fakeCmd) Run() error                         { _, err := f.CombinedOutput(); return err }
func (f *fakeCmd) Start() error                       { return nil }
func (f *fakeCmd) Wait() error                        { return nil }
func (f *fakeCmd) Stop()                              {}
func (f *fakeCmd) SetDir(dir string)                  {}
func (f *fakeCmd) SetStdin(in io.Reader)              {}
func (f *fakeCmd) SetStdout(out io.Writer)            {}
func (f *fakeCmd) SetStderr(out io.Writer)            {}
func (f *fakeCmd) SetEnv(env []string)                {}
func (f *fakeCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) SetSysProcAttr(attr interface{})    {}

func newFakeExec() *fakeExec {
	return &fakeExec{
		handlers: make(map[string]func(args ...string) ([]byte, error)),
	}
}

func (f *fakeExec) Command(cmd string, args ...string) k8sexec.Cmd {
	callStr := fmt.Sprintf("%s %s", cmd, strings.Join(args, " "))
	f.calls = append(f.calls, callStr)
	return &fakeCmd{
		cmd:     cmd,
		args:    args,
		handler: f.handlers[cmd],
	}
}

func (f *fakeExec) CommandContext(ctx context.Context, cmd string, args ...string) k8sexec.Cmd {
	return f.Command(cmd, args...)
}

func (f *fakeExec) LookPath(file string) (string, error) {
	return file, nil
}

// fakeExitError implements k8sexec.ExitError so tests can drive exit-status handling.
type fakeExitError struct {
	code int
}

func (e fakeExitError) String() string  { return fmt.Sprintf("exit status %d", e.code) }
func (e fakeExitError) Error() string   { return e.String() }
func (e fakeExitError) Exited() bool    { return true }
func (e fakeExitError) ExitStatus() int { return e.code }

func TestDiscoverDevice(t *testing.T) {
	tmpDir := t.TempDir()
	byIDDir := filepath.Join(tmpDir, "by-id")
	if err := os.MkdirAll(byIDDir, 0o750); err != nil {
		t.Fatalf("failed to create temp by-id dir: %v", err)
	}

	volID := "vol-12345678"
	targetFile := filepath.Join(byIDDir, "virtio-"+volID)
	if err := os.WriteFile(targetFile, []byte("fake-device"), 0o600); err != nil {
		t.Fatalf("failed to write dummy device: %v", err)
	}

	mounter := NewLinuxMounter(WithDiskByIDDir(byIDDir))
	ctx := context.Background()

	// Empty ID test
	_, err := mounter.DiscoverDevice(ctx, "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got: %v", err)
	}

	// Successful discovery with virtio prefix
	found, err := mounter.DiscoverDevice(ctx, volID)
	if err != nil {
		t.Fatalf("expected discovery success, got: %v", err)
	}
	if found != targetFile {
		t.Errorf("expected %s, got %s", targetFile, found)
	}

	// Absolute path existing file
	absFound, err := mounter.DiscoverDevice(ctx, targetFile)
	if err != nil {
		t.Fatalf("expected abs path discovery success, got: %v", err)
	}
	if absFound != targetFile {
		t.Errorf("expected %s, got %s", targetFile, absFound)
	}

	// Nonexistent device test
	_, err = mounter.DiscoverDevice(ctx, "nonexistent-vol")
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("expected ErrDeviceNotFound, got: %v", err)
	}
}

func TestDiscoverDeviceTruncatedVirtioSerial(t *testing.T) {
	tmpDir := t.TempDir()
	byIDDir := filepath.Join(tmpDir, "by-id")
	if err := os.MkdirAll(byIDDir, 0o750); err != nil {
		t.Fatalf("failed to create temp by-id dir: %v", err)
	}

	// udev truncates the volume UUID to the length of the VirtIO serial field.
	volumeID := "6f1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
	link := filepath.Join(byIDDir, "virtio-"+volumeID[:virtioSerialMaxLen])
	if err := os.WriteFile(link, []byte("fake-device"), 0o600); err != nil {
		t.Fatalf("failed to write dummy device: %v", err)
	}

	mounter := NewLinuxMounter(WithDiskByIDDir(byIDDir))

	found, err := mounter.DiscoverDevice(context.Background(), volumeID)
	if err != nil {
		t.Fatalf("expected discovery via truncated serial, got: %v", err)
	}
	if found != link {
		t.Errorf("expected %s, got %s", link, found)
	}
}

func TestGetFilesystemType(t *testing.T) {
	fe := newFakeExec()
	mounter := NewLinuxMounter(WithExecInterface(fe))
	ctx := context.Background()

	// Empty source
	_, err := mounter.GetFilesystemType(ctx, "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got: %v", err)
	}

	// Formatted ext4
	fe.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return []byte("ext4\n"), nil
	}
	fsType, err := mounter.GetFilesystemType(ctx, "/dev/sdb")
	if err != nil || fsType != "ext4" {
		t.Errorf("expected ext4, got fsType: %s, err: %v", fsType, err)
	}

	// Unformatted
	fe.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return []byte(""), nil
	}
	fsType, err = mounter.GetFilesystemType(ctx, "/dev/sdb")
	if err != nil || fsType != "" {
		t.Errorf("expected empty fsType for unformatted, got: %s, err: %v", fsType, err)
	}

	// blkid exit status 2 reports a device that carries no filesystem
	fe.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return []byte(""), fakeExitError{code: blkidNoFilesystem}
	}
	fsType, err = mounter.GetFilesystemType(ctx, "/dev/sdb")
	if err != nil || fsType != "" {
		t.Errorf("expected empty fsType for exit status 2, got: %s, err: %v", fsType, err)
	}

	// A failing blkid falls back to lsblk
	fe.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return []byte("blkid: command failed"), errors.New("blkid unavailable")
	}
	fe.handlers["lsblk"] = func(args ...string) ([]byte, error) {
		return []byte("xfs\n"), nil
	}
	fsType, err = mounter.GetFilesystemType(ctx, "/dev/sdb")
	if err != nil || fsType != "xfs" {
		t.Errorf("expected xfs from lsblk fallback, got: %s, err: %v", fsType, err)
	}

	// A device whose filesystem cannot be determined is an error, not an empty string,
	// so that callers never mistake it for an unformatted device and overwrite it.
	fe.handlers["lsblk"] = func(args ...string) ([]byte, error) {
		return nil, errors.New("lsblk unavailable")
	}
	fsType, err = mounter.GetFilesystemType(ctx, "/dev/sdb")
	if !errors.Is(err, ErrFilesystemDetectionFailed) {
		t.Errorf("expected ErrFilesystemDetectionFailed, got: %s, err: %v", fsType, err)
	}
}

func TestMount(t *testing.T) {
	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	fakeMounter := mountutils.NewFakeMounter(nil)
	fe := newFakeExec()
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(fe),
	)
	ctx := context.Background()

	// Input validation
	if err := mounter.Mount(ctx, "", targetPath, "", []string{"bind"}); !errors.Is(
		err,
		ErrInvalidInput,
	) {
		t.Errorf("expected ErrInvalidInput for empty source, got: %v", err)
	}
	if err := mounter.Mount(ctx, "/dev/sdb", "", "", []string{"bind"}); !errors.Is(
		err,
		ErrInvalidInput,
	) {
		t.Errorf("expected ErrInvalidInput for empty target, got: %v", err)
	}

	// A bind mount must not probe or format the source device.
	if err := mounter.Mount(ctx, "/dev/sdb", targetPath, "", []string{"bind"}); err != nil {
		t.Fatalf("expected Mount success, got: %v", err)
	}
	if len(fe.calls) != 0 {
		t.Errorf("expected Mount to run no external commands, got: %v", fe.calls)
	}
	if len(fakeMounter.MountPoints) != 1 {
		t.Fatalf("expected 1 mount point, got %d", len(fakeMounter.MountPoints))
	}

	mp := fakeMounter.MountPoints[0]
	if mp.Device != "/dev/sdb" || mp.Path != targetPath {
		t.Errorf("expected /dev/sdb at %s, got %s at %s", targetPath, mp.Device, mp.Path)
	}
	if !slices.Contains(mp.Opts, "bind") {
		t.Errorf("expected bind option to be passed through, got: %v", mp.Opts)
	}
	if mp.Type != "" {
		t.Errorf("expected no filesystem type for a bind mount, got: %q", mp.Type)
	}

	// Mounting an already mounted target is a no-op rather than a stacked mount.
	if err := mounter.Mount(ctx, "/dev/sdb", targetPath, "", []string{"bind"}); err != nil {
		t.Fatalf("expected idempotent Mount success, got: %v", err)
	}
	if len(fakeMounter.MountPoints) != 1 {
		t.Errorf(
			"expected target to stay mounted once, got %d mounts",
			len(fakeMounter.MountPoints),
		)
	}

	// A cancelled context is refused before touching the host.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mounter.Mount(cancelledCtx, "/dev/sdb", filepath.Join(tmpDir, "other"), "", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestIsMountPointAndGetMountSource(t *testing.T) {
	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	fakeMounter := mountutils.NewFakeMounter([]mountutils.MountPoint{
		{
			Device: "/dev/sdb",
			Path:   targetPath,
			Type:   "ext4",
		},
	})

	mounter := NewLinuxMounter(WithMountUtilsInterface(fakeMounter))
	ctx := context.Background()

	mounted, err := mounter.IsMountPoint(ctx, targetPath)
	if err != nil || !mounted {
		t.Errorf("expected target to be mount point, got mounted=%v, err=%v", mounted, err)
	}

	src, err := mounter.GetMountSource(ctx, targetPath)
	if err != nil || src != "/dev/sdb" {
		t.Errorf("expected mount source /dev/sdb, got %s, err=%v", src, err)
	}

	notMounted, err := mounter.IsMountPoint(ctx, filepath.Join(tmpDir, "nonexistent"))
	if err != nil || notMounted {
		t.Errorf("expected false for nonexistent path, got mounted=%v, err=%v", notMounted, err)
	}
}

func TestFormatAndMount(t *testing.T) {
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "target")
	devPath := "/dev/sdb"

	fakeMounter := mountutils.NewFakeMounter(nil)
	fe := newFakeExec()
	fe.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return []byte(""), nil
	}
	fe.handlers["lsblk"] = func(args ...string) ([]byte, error) {
		return []byte(""), nil
	}
	fe.handlers["mkfs.ext4"] = func(args ...string) ([]byte, error) {
		return []byte("done"), nil
	}
	fe.handlers["mkfs.xfs"] = func(args ...string) ([]byte, error) {
		return []byte("done"), nil
	}

	mounter := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(fe),
	)
	ctx := context.Background()

	// Unsupported filesystem test
	err := mounter.FormatAndMount(ctx, devPath, targetDir, "btrfs", nil)
	if !errors.Is(err, ErrUnsupportedFilesystem) {
		t.Errorf("expected ErrUnsupportedFilesystem, got %v", err)
	}

	// Format and mount ext4
	err = mounter.FormatAndMount(ctx, devPath, targetDir, "ext4", []string{"discard", "defaults"})
	if err != nil {
		t.Fatalf("expected FormatAndMount ext4 success, got: %v", err)
	}

	// Format and mount xfs
	xfsTargetDir := filepath.Join(tmpDir, "target-xfs")
	err = mounter.FormatAndMount(ctx, devPath, xfsTargetDir, "xfs", []string{"defaults"})
	if err != nil {
		t.Fatalf("expected FormatAndMount xfs success, got: %v", err)
	}

	// Filesystem mismatch test
	mismatchExec := newFakeExec()
	mismatchExec.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return []byte("TYPE=\"xfs\"\n"), nil
	}
	mounterMismatch := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(mismatchExec),
	)
	err = mounterMismatch.FormatAndMount(
		ctx,
		devPath,
		filepath.Join(tmpDir, "target2"),
		"ext4",
		nil,
	)
	if !errors.Is(err, ErrFilesystemMismatch) {
		t.Errorf("expected ErrFilesystemMismatch, got %v", err)
	}

	// A device whose filesystem cannot be determined must not be formatted.
	undetectableExec := newFakeExec()
	undetectableExec.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return nil, errors.New("blkid unavailable")
	}
	undetectableExec.handlers["lsblk"] = func(args ...string) ([]byte, error) {
		return nil, errors.New("lsblk unavailable")
	}
	mounterUndetectable := NewLinuxMounter(
		WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
		WithExecInterface(undetectableExec),
	)
	err = mounterUndetectable.FormatAndMount(
		ctx,
		devPath,
		filepath.Join(tmpDir, "target3"),
		"ext4",
		nil,
	)
	if !errors.Is(err, ErrFilesystemDetectionFailed) {
		t.Errorf("expected ErrFilesystemDetectionFailed, got %v", err)
	}
	for _, call := range undetectableExec.calls {
		if strings.HasPrefix(call, "mkfs") {
			t.Errorf("expected no format attempt when detection fails, got call %q", call)
		}
	}
}

func TestUnmount(t *testing.T) {
	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	fakeMounter := mountutils.NewFakeMounter([]mountutils.MountPoint{
		{
			Device: "/dev/sdb",
			Path:   targetPath,
			Type:   "ext4",
		},
	})

	mounter := NewLinuxMounter(WithMountUtilsInterface(fakeMounter))
	ctx := context.Background()

	// Successful unmount
	err := mounter.Unmount(ctx, targetPath)
	if err != nil {
		t.Fatalf("expected unmount success, got: %v", err)
	}

	// Idempotent unmount when target is no longer mounted
	err = mounter.Unmount(ctx, targetPath)
	if err != nil {
		t.Errorf("expected idempotent unmount success, got: %v", err)
	}

	// Idempotent unmount when target directory does not exist
	err = mounter.Unmount(ctx, filepath.Join(tmpDir, "nonexistent"))
	if err != nil {
		t.Errorf("expected idempotent unmount success for missing path, got: %v", err)
	}
}

func TestDefaultNewLinuxMounter(t *testing.T) {
	m := NewLinuxMounter()
	if m == nil || m.safeMounter == nil {
		t.Fatal("expected non-nil LinuxMounter and safeMounter")
	}
}
