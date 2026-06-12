package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mountutils "k8s.io/mount-utils"
	k8sexec "k8s.io/utils/exec"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/mount/mounttest"
)

type fakeExec struct {
	handlers map[string]func(args ...string) ([]byte, error)
	// blocking commands hang until their context is done.
	blocking map[string]bool
	// started receives each command line when non-nil (buffered to avoid test deadlocks).
	started chan string
	// mu guards calls and envs, which the cancellation tests record from a second goroutine.
	mu    sync.Mutex
	calls []string
	// envs holds the environment each command was given, one entry per command.
	envs [][]string
}

type fakeCmd struct {
	ctx     context.Context
	exec    *fakeExec
	cmd     string
	args    []string
	handler func(args ...string) ([]byte, error)
	block   bool
}

func (f *fakeCmd) CombinedOutput() ([]byte, error) {
	if f.block {
		<-f.ctx.Done()
		return nil, f.ctx.Err()
	}
	if f.handler != nil {
		return f.handler(f.args...)
	}
	return nil, nil
}
func (f *fakeCmd) Output() ([]byte, error) { return f.CombinedOutput() }
func (f *fakeCmd) Run() error              { _, err := f.CombinedOutput(); return err }
func (f *fakeCmd) Start() error            { return nil }
func (f *fakeCmd) Wait() error             { return nil }
func (f *fakeCmd) Stop()                   {}
func (f *fakeCmd) SetDir(dir string)       {}
func (f *fakeCmd) SetStdin(in io.Reader)   {}
func (f *fakeCmd) SetStdout(out io.Writer) {}
func (f *fakeCmd) SetStderr(out io.Writer) {}
func (f *fakeCmd) SetEnv(env []string) {
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	f.exec.envs = append(f.exec.envs, env)
}
func (f *fakeCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) SetSysProcAttr(attr any)            {}

func newFakeExec() *fakeExec {
	return &fakeExec{
		handlers: make(map[string]func(args ...string) ([]byte, error)),
		blocking: make(map[string]bool),
	}
}

func (f *fakeExec) Command(cmd string, args ...string) k8sexec.Cmd {
	return f.command(context.Background(), cmd, args...)
}

func (f *fakeExec) CommandContext(ctx context.Context, cmd string, args ...string) k8sexec.Cmd {
	return f.command(ctx, cmd, args...)
}

func (f *fakeExec) command(ctx context.Context, cmd string, args ...string) k8sexec.Cmd {
	callStr := fmt.Sprintf("%s %s", cmd, strings.Join(args, " "))
	f.mu.Lock()
	f.calls = append(f.calls, callStr)
	f.mu.Unlock()
	if f.started != nil {
		f.started <- callStr
	}
	return &fakeCmd{
		ctx:     ctx,
		exec:    f,
		cmd:     cmd,
		args:    args,
		handler: f.handlers[cmd],
		block:   f.blocking[cmd],
	}
}

func (f *fakeExec) LookPath(file string) (string, error) {
	return file, nil
}

func (f *fakeExec) recordedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeExec) recordedEnvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.envs)
}

// withMountTable wires mount/umount fakes so the table stays in sync with exec.
func (f *fakeExec) withMountTable(fakeMounter *mountutils.FakeMounter) *fakeExec {
	maps.Copy(f.handlers, mounttest.Commands(fakeMounter))
	return f
}

// trackStarts records each command as it is created.
func (f *fakeExec) trackStarts() *fakeExec {
	f.started = make(chan string, 16)
	return f
}

// waitForCommand blocks until a command starting with prefix is created.
func waitForCommand(t *testing.T, fe *fakeExec, prefix string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case call := <-fe.started:
			if strings.HasPrefix(call, prefix) {
				return
			}
		case <-deadline:
			t.Fatalf("%s did not start; cannot test in-flight cancellation", prefix)
		}
	}
}

// fakeExitError implements k8sexec.ExitError.
type fakeExitError struct {
	code int
}

func (e fakeExitError) String() string  { return fmt.Sprintf("exit status %d", e.code) }
func (e fakeExitError) Error() string   { return e.String() }
func (e fakeExitError) Exited() bool    { return true }
func (e fakeExitError) ExitStatus() int { return e.code }

// newDeviceTree builds a fake /dev + by-id tree for discovery tests.
func newDeviceTree(t *testing.T) (*LinuxMounter, string, string, string) {
	t.Helper()

	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to resolve temporary directory: %v", err)
	}

	devDir := filepath.Join(tmpDir, "dev")
	byIDDir := filepath.Join(devDir, "disk", "by-id")
	if err := os.MkdirAll(byIDDir, 0o750); err != nil {
		t.Fatalf("failed to create temp by-id dir: %v", err)
	}

	device := filepath.Join(devDir, "vdb")
	if err := os.WriteFile(device, []byte("fake-device"), 0o600); err != nil {
		t.Fatalf("failed to write dummy device: %v", err)
	}

	mounter := NewLinuxMounter(WithDevDir(devDir), WithDiskByIDDir(byIDDir))

	return mounter, devDir, byIDDir, device
}

func TestDiscoverDevice(t *testing.T) {
	t.Parallel()

	mounter, _, byIDDir, device := newDeviceTree(t)
	ctx := t.Context()

	volID := "vol-12345678"
	link := filepath.Join(byIDDir, "virtio-"+volID)
	if err := os.Symlink(device, link); err != nil {
		t.Fatalf("failed to link the attached device: %v", err)
	}

	// Empty volume ID test
	_, err := mounter.DiscoverDevice(ctx, "", device)
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got: %v", err)
	}

	// Empty published path is invalid.
	_, err = mounter.DiscoverDevice(ctx, volID, "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for an empty published path, got: %v", err)
	}

	// Successful discovery: the by-id link and the published path name the same device
	found, err := mounter.DiscoverDevice(ctx, volID, device)
	if err != nil {
		t.Fatalf("expected discovery success, got: %v", err)
	}
	if found != device {
		t.Errorf("expected %s, got %s", device, found)
	}

	_, err = mounter.DiscoverDevice(ctx, "nonexistent-vol", device)
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("expected ErrDeviceNotFound, got: %v", err)
	}
}

func TestDiscoverDeviceTruncatedVirtioSerial(t *testing.T) {
	t.Parallel()

	mounter, _, byIDDir, device := newDeviceTree(t)

	// udev truncates volume UUIDs to the VirtIO serial field length.
	volumeID := "6f1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
	link := filepath.Join(byIDDir, "virtio-"+volumeID[:virtioSerialMaxLen])
	if err := os.Symlink(device, link); err != nil {
		t.Fatalf("failed to link the attached device: %v", err)
	}

	found, err := mounter.DiscoverDevice(t.Context(), volumeID, device)
	if err != nil {
		t.Fatalf("expected discovery via an agreeing truncated serial, got: %v", err)
	}
	if found != device {
		t.Errorf("expected %s, got %s", device, found)
	}
}

func TestDiscoverDeviceRejectsDisagreeingIdentity(t *testing.T) {
	t.Parallel()

	mounter, devDir, byIDDir, device := newDeviceTree(t)

	// by-id link points at a different device than the published path.
	other := filepath.Join(devDir, "vdc")
	if err := os.WriteFile(other, []byte("other-volume"), 0o600); err != nil {
		t.Fatalf("failed to write second device: %v", err)
	}
	volID := "vol-12345678"
	link := filepath.Join(byIDDir, "virtio-"+volID)
	if err := os.Symlink(other, link); err != nil {
		t.Fatalf("failed to link the other device: %v", err)
	}

	_, err := mounter.DiscoverDevice(t.Context(), volID, device)
	if !errors.Is(err, ErrDeviceIdentityUnverified) {
		t.Errorf("expected ErrDeviceIdentityUnverified, got: %v", err)
	}
}

func TestDiscoverDeviceRejectsPathOutsideDeviceDirectory(t *testing.T) {
	t.Parallel()

	mounter, devDir, byIDDir, device := newDeviceTree(t)

	volID := "vol-12345678"
	link := filepath.Join(byIDDir, "virtio-"+volID)
	if err := os.Symlink(device, link); err != nil {
		t.Fatalf("failed to link the attached device: %v", err)
	}

	// Paths outside the device dir are invalid.
	outside := filepath.Join(filepath.Dir(devDir), "not-a-device")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("failed to write outside file: %v", err)
	}

	_, err := mounter.DiscoverDevice(t.Context(), volID, outside)
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for a path outside %s, got: %v", devDir, err)
	}
}

func TestDiscoverDeviceRejectsAbsentPublishedPath(t *testing.T) {
	t.Parallel()

	mounter, devDir, byIDDir, device := newDeviceTree(t)

	volID := "vol-12345678"
	link := filepath.Join(byIDDir, "virtio-"+volID)
	if err := os.Symlink(device, link); err != nil {
		t.Fatalf("failed to link the attached device: %v", err)
	}

	_, err := mounter.DiscoverDevice(t.Context(), volID, filepath.Join(devDir, "vdz"))
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("expected ErrDeviceNotFound for an absent published path, got: %v", err)
	}
}

func TestGetFilesystemType(t *testing.T) {
	t.Parallel()

	fe := newFakeExec()
	mounter := NewLinuxMounter(WithExecInterface(fe))
	ctx := t.Context()

	_, err := mounter.GetFilesystemType(ctx, "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got: %v", err)
	}

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

	// blkid exit 2: no filesystem
	fe.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return []byte(""), fakeExitError{code: blkidNoFilesystem}
	}
	fsType, err = mounter.GetFilesystemType(ctx, "/dev/sdb")
	if err != nil || fsType != "" {
		t.Errorf("expected empty fsType for exit status 2, got: %s, err: %v", fsType, err)
	}

	// blkid failure falls back to lsblk
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

	fe.handlers["lsblk"] = func(args ...string) ([]byte, error) {
		return nil, errors.New("lsblk unavailable")
	}
	fsType, err = mounter.GetFilesystemType(ctx, "/dev/sdb")
	if !errors.Is(err, ErrFilesystemDetectionFailed) {
		t.Errorf("expected ErrFilesystemDetectionFailed, got: %s, err: %v", fsType, err)
	}
}

func TestMount(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	fakeMounter := mountutils.NewFakeMounter(nil)
	fe := newFakeExec().withMountTable(fakeMounter)
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(fe),
		// Source is ro,nosuid; bind remount must keep both.
		WithStatfsFunc(mounttest.StaticStatfs(statfsReadOnly|statfsNoSUID)),
	)
	ctx := t.Context()

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

	// Bind then remount: kernel ignores other flags with MS_BIND.
	if err := mounter.Mount(ctx, "/dev/sdb", targetPath, "", []string{"bind"}); err != nil {
		t.Fatalf("expected Mount success, got: %v", err)
	}
	wantCalls := []string{
		"mount -o bind /dev/sdb " + targetPath,
		"mount -o bind,remount,ro,nosuid /dev/sdb " + targetPath,
	}
	if got := fe.recordedCalls(); !slices.Equal(got, wantCalls) {
		t.Errorf("expected the two-step bind %v, got: %v", wantCalls, got)
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
	for _, option := range []string{"ro", "nosuid"} {
		if !slices.Contains(mp.Opts, option) {
			t.Errorf("expected the bind to carry the source %s flag, got: %v", option, mp.Opts)
		}
	}
	if mp.Type != "" {
		t.Errorf("expected no filesystem type for a bind mount, got: %q", mp.Type)
	}

	// Already mounted: remount only (no second bind).
	if err := mounter.Mount(ctx, "/dev/sdb", targetPath, "", []string{"bind"}); err != nil {
		t.Fatalf("expected idempotent Mount success, got: %v", err)
	}
	if len(fakeMounter.MountPoints) != 1 {
		t.Errorf(
			"expected target to stay mounted once, got %d mounts",
			len(fakeMounter.MountPoints),
		)
	}
	wantRepeated := append(slices.Clone(wantCalls), wantCalls[1])
	if got := fe.recordedCalls(); !slices.Equal(got, wantRepeated) {
		t.Errorf("expected the repeated Mount to repeat only the remount %v, got: %v",
			wantRepeated, got)
	}

	cancelledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	err := mounter.Mount(cancelledCtx, "/dev/sdb", filepath.Join(tmpDir, "other"), "", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

// Failed remount must leave the target unmounted so retry is not a false success.
func TestBindRollsBackWhenTheRemountFails(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	fakeMounter := mountutils.NewFakeMounter(nil)
	fe := newFakeExec().withMountTable(fakeMounter)
	remountFails := true
	mountTable := fe.handlers["mount"]
	fe.handlers["mount"] = func(args ...string) ([]byte, error) {
		if remountFails && strings.Contains(strings.Join(args, " "), "remount") {
			return []byte("mount: permission denied"), fakeExitError{code: 32}
		}
		return mountTable(args...)
	}
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(fe),
		WithStatfsFunc(mounttest.StaticStatfs(0)), // a read-write source
	)

	options := []string{"bind", "ro"}
	if err := mounter.Mount(t.Context(), "/dev/sdb", targetPath, "", options); !errors.Is(
		err,
		ErrMountFailed,
	) {
		t.Fatalf("expected ErrMountFailed when the remount fails, got: %v", err)
	}
	if len(fakeMounter.MountPoints) != 0 {
		t.Fatalf("the failed bind left %+v mounted", fakeMounter.MountPoints)
	}

	// The retry finds a clean target and completes the publish the failure abandoned.
	remountFails = false
	if err := mounter.Mount(t.Context(), "/dev/sdb", targetPath, "", options); err != nil {
		t.Fatalf("expected the retry to publish the volume, got: %v", err)
	}
	if len(fakeMounter.MountPoints) != 1 {
		t.Fatalf("expected one mount after the retry, got %+v", fakeMounter.MountPoints)
	}
	if opts := fakeMounter.MountPoints[0].Opts; !slices.Contains(opts, "ro") {
		t.Errorf("the retried publish is not read-only, options: %v", opts)
	}
}

// Remount a bind that is mounted but missing requested options.
func TestBindRepairsATargetMissingItsOptions(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	// Bound without the requested options (interrupted publish).
	fakeMounter := mountutils.NewFakeMounter([]mountutils.MountPoint{
		{Device: "/dev/sdb", Path: targetPath, Opts: []string{"bind"}},
	})
	fe := newFakeExec().withMountTable(fakeMounter)
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(fe),
		WithStatfsFunc(mounttest.StaticStatfs(0)),
	)

	if err := mounter.Mount(
		t.Context(),
		"/dev/sdb",
		targetPath,
		"",
		[]string{"bind", "ro"},
	); err != nil {
		t.Fatalf("expected Mount to repair the target, got: %v", err)
	}

	wantCalls := []string{"mount -o bind,remount,ro /dev/sdb " + targetPath}
	if got := fe.recordedCalls(); !slices.Equal(got, wantCalls) {
		t.Errorf("expected only the remount %v, got: %v", wantCalls, got)
	}
	if len(fakeMounter.MountPoints) != 1 {
		t.Fatalf("expected the target to stay mounted once, got %+v", fakeMounter.MountPoints)
	}
	if opts := fakeMounter.MountPoints[0].Opts; !slices.Contains(opts, "ro") {
		t.Errorf("the repaired target is not read-only, options: %v", opts)
	}
}

// Conflicting remount options: last listed wins; ro source is never widened.
func TestBindRemountOptionPrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		sourceFlags int64
		options     []string
		wantRemount string
	}{
		{
			name:        "requested option wins over source flag",
			sourceFlags: statfsRelATime,
			options:     []string{"bind", "noatime"},
			wantRemount: "mount -o bind,remount,relatime,noatime /dev/sdb ",
		},
		{
			name:        "ro request on rw source",
			sourceFlags: statfsRelATime,
			options:     []string{"bind", "ro"},
			wantRemount: "mount -o bind,remount,relatime,ro /dev/sdb ",
		},
		{
			name:        "rw request on ro source stays ro",
			sourceFlags: statfsReadOnly,
			options:     []string{"bind", "rw"},
			wantRemount: "mount -o bind,remount,ro /dev/sdb ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			targetPath := filepath.Join(tmpDir, "target")
			if err := os.MkdirAll(targetPath, 0o750); err != nil {
				t.Fatalf("failed to create target dir: %v", err)
			}

			fakeMounter := mountutils.NewFakeMounter(nil)
			fe := newFakeExec().withMountTable(fakeMounter)
			mounter := NewLinuxMounter(
				WithMountUtilsInterface(fakeMounter),
				WithExecInterface(fe),
				WithStatfsFunc(mounttest.StaticStatfs(tc.sourceFlags)),
			)

			if err := mounter.Mount(
				t.Context(),
				"/dev/sdb",
				targetPath,
				"",
				tc.options,
			); err != nil {
				t.Fatalf("expected Mount success, got: %v", err)
			}

			calls := fe.recordedCalls()
			if len(calls) != 2 {
				t.Fatalf("expected a bind and a remount, got: %v", calls)
			}
			if want := tc.wantRemount + targetPath; calls[1] != want {
				t.Errorf("expected the remount %q, got: %q", want, calls[1])
			}
		})
	}
}

// Remount restates every per-mount source flag; not superblock flags (sync, mandlock).
func TestBindCarriesEveryPerMountSourceFlag(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	// Superblock flags (not restated on remount).
	const synchronous, mandLock = 0x0010, 0x0040

	fakeMounter := mountutils.NewFakeMounter(nil)
	fe := newFakeExec().withMountTable(fakeMounter)
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(fe),
		// All per-mount flags plus superblock flags above.
		WithStatfsFunc(mounttest.StaticStatfs(
			statfsReadOnly|statfsNoSUID|statfsNoDev|statfsNoExec|
				statfsNoATime|statfsNoDirATime|statfsRelATime|statfsNoSymFollow|
				synchronous|mandLock,
		)),
	)

	if err := mounter.Mount(t.Context(), "/dev/sdb", targetPath, "", []string{"bind"}); err != nil {
		t.Fatalf("expected Mount success, got: %v", err)
	}

	wantCalls := []string{
		"mount -o bind /dev/sdb " + targetPath,
		"mount -o bind,remount,ro,nosuid,nodev,noexec,noatime,nodiratime,relatime,nosymfollow " +
			"/dev/sdb " + targetPath,
	}
	if got := fe.recordedCalls(); !slices.Equal(got, wantCalls) {
		t.Errorf("expected the remount to repeat %v, got: %v", wantCalls, got)
	}
}

// rbind also uses bind-then-remount; remount uses plain bind.
func TestRecursiveBindTakesTheTwoStepPath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	fakeMounter := mountutils.NewFakeMounter(nil)
	fe := newFakeExec().withMountTable(fakeMounter)
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(fe),
		WithStatfsFunc(mounttest.StaticStatfs(0)), // a read-write source
	)

	if err := mounter.Mount(
		t.Context(),
		"/dev/sdb",
		targetPath,
		"",
		[]string{"rbind", "ro"},
	); err != nil {
		t.Fatalf("expected Mount success, got: %v", err)
	}

	// First step keeps rbind; remount uses plain bind (flags apply to the named mount only).
	wantCalls := []string{
		"mount -o rbind /dev/sdb " + targetPath,
		"mount -o bind,remount,ro /dev/sdb " + targetPath,
	}
	if got := fe.recordedCalls(); !slices.Equal(got, wantCalls) {
		t.Errorf("expected the two-step recursive bind %v, got: %v", wantCalls, got)
	}
	if len(fakeMounter.MountPoints) != 1 {
		t.Fatalf("expected one mount, got %+v", fakeMounter.MountPoints)
	}
	if opts := fakeMounter.MountPoints[0].Opts; !slices.Contains(opts, "ro") {
		t.Errorf("the recursive bind is not read-only, options: %v", opts)
	}
}

// Host commands set LC_ALL=C so output matching is locale-stable.
func TestHostCommandsRunInTheCLocale(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	fakeMounter := mountutils.NewFakeMounter([]mountutils.MountPoint{
		{Device: "/dev/sdb", Path: targetPath, Type: "ext4"},
	})
	fe := newFakeExec()
	// Case variant of "not mounted" still matches after lowercasing.
	fe.handlers["umount"] = func(args ...string) ([]byte, error) {
		return []byte("umount: " + args[0] + ": NOT MOUNTED."), fakeExitError{code: 32}
	}
	mounter := NewLinuxMounter(WithMountUtilsInterface(fakeMounter), WithExecInterface(fe))

	if err := mounter.Unmount(t.Context(), targetPath); err != nil {
		t.Errorf("expected an already unmounted target to succeed, got: %v", err)
	}

	envs := fe.recordedEnvs()
	if len(envs) == 0 {
		t.Fatal("no host command was given an environment")
	}
	for _, env := range envs {
		if !slices.Contains(env, "LC_ALL=C") {
			t.Errorf("a host command ran without the pinned locale, environment: %v", env)
		}
	}
}

func TestIsMountPointAndGetMountSource(t *testing.T) {
	t.Parallel()

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
	ctx := t.Context()

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

	cancelledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := mounter.IsMountPoint(cancelledCtx, targetPath); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Errorf("expected context.Canceled from IsMountPoint, got: %v", err)
	}
	if _, err := mounter.GetMountSource(cancelledCtx, targetPath); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Errorf("expected context.Canceled from GetMountSource, got: %v", err)
	}
}

func TestFormatAndMount(t *testing.T) {
	t.Parallel()

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
	ctx := t.Context()

	err := mounter.FormatAndMount(ctx, devPath, targetDir, "btrfs", nil)
	if !errors.Is(err, ErrUnsupportedFilesystem) {
		t.Errorf("expected ErrUnsupportedFilesystem, got %v", err)
	}

	err = mounter.FormatAndMount(ctx, devPath, targetDir, "ext4", []string{"discard", "defaults"})
	if err != nil {
		t.Fatalf("expected FormatAndMount ext4 success, got: %v", err)
	}

	xfsTargetDir := filepath.Join(tmpDir, "target-xfs")
	err = mounter.FormatAndMount(ctx, devPath, xfsTargetDir, "xfs", []string{"defaults"})
	if err != nil {
		t.Fatalf("expected FormatAndMount xfs success, got: %v", err)
	}

	mismatchExec := newFakeExec()
	// blkid -o value -s TYPE returns the bare type only.
	mismatchExec.handlers["blkid"] = func(args ...string) ([]byte, error) {
		return []byte("xfs\n"), nil
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

	// Skip format when filesystem detection fails.
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
	for _, call := range undetectableExec.recordedCalls() {
		if strings.HasPrefix(call, "mkfs") {
			t.Errorf("expected no format attempt when detection fails, got call %q", call)
		}
	}
}

// IsSupportedFilesystemType and FormatAndMount must agree on supported names.
func TestSupportedFilesystemAgreement(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	names := []string{"ext4", "xfs", "EXT4", "btrfs", "zfs", "ntfs"}
	for i, name := range names {
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
			WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
			WithExecInterface(fe),
		)

		err := mounter.FormatAndMount(
			t.Context(),
			"/dev/sdb",
			filepath.Join(tmpDir, fmt.Sprintf("agree-target-%d", i)),
			name,
			nil,
		)
		refusedByMounter := errors.Is(err, ErrUnsupportedFilesystem)
		if IsSupportedFilesystemType(name) == refusedByMounter {
			t.Errorf(
				"predicate and mounter disagree about %q: supported=%v, mounter error=%v",
				name,
				IsSupportedFilesystemType(name),
				err,
			)
		}
	}
}

func TestUnmount(t *testing.T) {
	t.Parallel()

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
	fe := newFakeExec().withMountTable(fakeMounter)

	mounter := NewLinuxMounter(WithMountUtilsInterface(fakeMounter), WithExecInterface(fe))
	ctx := t.Context()

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
	t.Parallel()

	m := NewLinuxMounter()
	if m == nil || m.mounter == nil || m.exec == nil {
		t.Fatal("expected a LinuxMounter with a mount table parser and exec")
	}
}

// A cancelled probe reports the context cause and starts no fallback probe.
func TestFilesystemProbeCancellationReportsTheContextCause(t *testing.T) {
	t.Parallel()

	fe := newFakeExec().trackStarts()
	fe.blocking["blkid"] = true
	// A killed fallback probe fails the way the host would.
	fe.handlers["lsblk"] = func(_ ...string) ([]byte, error) {
		return nil, errors.New("lsblk: killed")
	}
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
		WithExecInterface(fe),
	)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := mounter.GetFilesystemType(ctx, "/dev/sdb")
		result <- err
	}()

	waitForCommand(t, fe, "blkid")
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled for an interrupted probe, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled in-flight probe did not return; the host command is unbounded")
	}

	calls := fe.recordedCalls()
	for _, call := range calls {
		if strings.HasPrefix(call, "lsblk") {
			t.Errorf("the cancelled probe started a second host command: %v", calls)
		}
	}
}

func TestMountCancellationStopsInFlightCommand(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	fe := newFakeExec().trackStarts()
	fe.blocking["mount"] = true
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
		WithExecInterface(fe),
	)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- mounter.Mount(ctx, "/dev/sdb", targetPath, "", []string{"bind"})
	}()

	waitForCommand(t, fe, "mount")
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled for an interrupted mount, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled in-flight mount did not return; the host command is unbounded")
	}
}

func TestFormatAndMountCancellationStopsInFlightFormat(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "target")

	fe := newFakeExec().trackStarts()
	fe.handlers["blkid"] = func(_ ...string) ([]byte, error) {
		return []byte(""), nil
	}
	fe.blocking["mkfs.ext4"] = true
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
		WithExecInterface(fe),
	)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- mounter.FormatAndMount(ctx, "/dev/sdb", targetDir, "ext4", nil)
	}()

	// blkid runs first; wait until mkfs is in flight.
	waitForCommand(t, fe, "mkfs.ext4")
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled for an interrupted format, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled in-flight format did not return; the host command is unbounded")
	}

	calls := fe.recordedCalls()
	for _, call := range calls {
		if strings.HasPrefix(call, "mount ") {
			t.Errorf("mount attempted after the format was interrupted: %v", calls)
		}
	}
}

func TestUnmountDeadlineBoundsHungCommand(t *testing.T) {
	t.Parallel()

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
	fe := newFakeExec()
	fe.blocking["umount"] = true
	mounter := NewLinuxMounter(WithMountUtilsInterface(fakeMounter), WithExecInterface(fe))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := mounter.Unmount(ctx, targetPath)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded for a hung umount, got: %v", err)
	}
	// Bound tightly so a missed deadline still fails this test.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a hung umount blocked %v beyond the caller's deadline", elapsed)
	}
}

func TestHostCommandFailuresKeepSentinels(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	ctx := t.Context()

	mountedTable := func() *mountutils.FakeMounter {
		return mountutils.NewFakeMounter([]mountutils.MountPoint{
			{Device: "/dev/sdb", Path: targetPath, Type: "ext4"},
		})
	}

	t.Run("a failed mount keeps its sentinel and output", func(t *testing.T) {
		t.Parallel()

		fe := newFakeExec()
		fe.handlers["mount"] = func(_ ...string) ([]byte, error) {
			return []byte("mount: permission denied"), fakeExitError{code: 32}
		}
		mounter := NewLinuxMounter(
			WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
			WithExecInterface(fe),
		)

		err := mounter.Mount(ctx, "/dev/sdb", targetPath, "", []string{"bind"})
		if !errors.Is(err, ErrMountFailed) {
			t.Fatalf("expected ErrMountFailed for a failed mount, got: %v", err)
		}
		if !strings.Contains(err.Error(), "mount: permission denied") {
			t.Errorf("expected the mount output to be surfaced, got: %v", err)
		}
	})

	t.Run("a failed umount keeps its sentinel and output", func(t *testing.T) {
		t.Parallel()

		fe := newFakeExec()
		fe.handlers["umount"] = func(_ ...string) ([]byte, error) {
			return []byte("umount: target is busy"), fakeExitError{code: 32}
		}
		mounter := NewLinuxMounter(
			WithMountUtilsInterface(mountedTable()),
			WithExecInterface(fe),
		)

		err := mounter.Unmount(ctx, targetPath)
		if !errors.Is(err, ErrUnmountFailed) {
			t.Fatalf("expected ErrUnmountFailed for a failed umount, got: %v", err)
		}
		if !strings.Contains(err.Error(), "umount: target is busy") {
			t.Errorf("expected the umount output to be surfaced, got: %v", err)
		}
	})

	t.Run("an already unmounted target stays successful", func(t *testing.T) {
		t.Parallel()

		fe := newFakeExec()
		fe.handlers["umount"] = func(args ...string) ([]byte, error) {
			return []byte("umount: " + args[0] + ": not mounted."), fakeExitError{code: 32}
		}
		mounter := NewLinuxMounter(
			WithMountUtilsInterface(mountedTable()),
			WithExecInterface(fe),
		)

		if err := mounter.Unmount(ctx, targetPath); err != nil {
			t.Errorf("expected an already unmounted target to succeed, got: %v", err)
		}
	})

	t.Run("a failed format keeps its sentinel and output", func(t *testing.T) {
		t.Parallel()

		fe := newFakeExec()
		fe.handlers["blkid"] = func(_ ...string) ([]byte, error) { return []byte(""), nil }
		fe.handlers["mkfs.ext4"] = func(_ ...string) ([]byte, error) {
			return []byte("mkfs.ext4: device is busy"), fakeExitError{code: 1}
		}
		mounter := NewLinuxMounter(
			WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
			WithExecInterface(fe),
		)

		err := mounter.FormatAndMount(
			ctx,
			"/dev/sdb",
			filepath.Join(tmpDir, "unformattable"),
			"ext4",
			nil,
		)
		if !errors.Is(err, ErrFilesystemFormatFailed) {
			t.Fatalf("expected ErrFilesystemFormatFailed for a failed mkfs, got: %v", err)
		}
		if !strings.Contains(err.Error(), "mkfs.ext4: device is busy") {
			t.Errorf("expected the mkfs output to be surfaced, got: %v", err)
		}
	})

	t.Run("uncorrectable fsck errors fail the mount", func(t *testing.T) {
		t.Parallel()

		fe := newFakeExec()
		fe.handlers["blkid"] = func(_ ...string) ([]byte, error) { return []byte("ext4\n"), nil }
		fe.handlers["fsck"] = func(_ ...string) ([]byte, error) {
			return []byte("fsck: uncorrectable errors"), fakeExitError{
				code: fsckErrorsUncorrected,
			}
		}
		mounter := NewLinuxMounter(
			WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
			WithExecInterface(fe),
		)

		err := mounter.FormatAndMount(
			ctx,
			"/dev/sdb",
			filepath.Join(tmpDir, "corrupted"),
			"ext4",
			nil,
		)
		if !errors.Is(err, ErrMountFailed) {
			t.Fatalf(
				"expected ErrMountFailed when fsck cannot correct the filesystem, got: %v",
				err,
			)
		}
		if !strings.Contains(err.Error(), "fsck: uncorrectable errors") {
			t.Errorf("expected the fsck output to be surfaced, got: %v", err)
		}
	})

	t.Run("every other fsck outcome is tolerated", func(t *testing.T) {
		t.Parallel()

		fakeMounter := mountutils.NewFakeMounter(nil)
		fe := newFakeExec().withMountTable(fakeMounter)
		fe.handlers["blkid"] = func(_ ...string) ([]byte, error) { return []byte("ext4\n"), nil }
		fe.handlers["fsck"] = func(_ ...string) ([]byte, error) {
			return nil, errors.New("fsck: executable not found")
		}
		mounter := NewLinuxMounter(
			WithMountUtilsInterface(fakeMounter),
			WithExecInterface(fe),
		)

		if err := mounter.FormatAndMount(
			ctx,
			"/dev/sdb",
			filepath.Join(tmpDir, "tolerated"),
			"ext4",
			nil,
		); err != nil {
			t.Errorf("expected a missing fsck to be tolerated, got: %v", err)
		}
	})

	t.Run("an unformatted read-only stage formats nothing", func(t *testing.T) {
		t.Parallel()

		fe := newFakeExec()
		fe.handlers["blkid"] = func(_ ...string) ([]byte, error) { return []byte(""), nil }
		mounter := NewLinuxMounter(
			WithMountUtilsInterface(mountutils.NewFakeMounter(nil)),
			WithExecInterface(fe),
		)

		err := mounter.FormatAndMount(
			ctx,
			"/dev/sdb",
			filepath.Join(tmpDir, "read-only"),
			"ext4",
			[]string{"ro"},
		)
		if !errors.Is(err, ErrMountFailed) {
			t.Fatalf("expected ErrMountFailed for an unformatted read-only mount, got: %v", err)
		}
		calls := fe.recordedCalls()
		for _, call := range calls {
			if strings.HasPrefix(call, "mkfs") {
				t.Errorf("a read-only mount formatted the device: %v", calls)
			}
		}
	})
}

// Exact mkfs/fsck/mount argv shapes for stage.
func TestFormatAndMountCommandShapes(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cases := []struct {
		name      string
		fsType    string
		blkid     string
		wantCalls []string
	}{
		{
			name:   "unformatted device is formatted then mounted",
			fsType: "ext4",
			blkid:  "",
			wantCalls: []string{
				"blkid -o value -s TYPE /dev/sdb",
				"mkfs.ext4 -F -m0 /dev/sdb",
				"mount -t ext4 -o defaults /dev/sdb %s",
			},
		},
		{
			name:   "xfs format args",
			fsType: "xfs",
			blkid:  "",
			wantCalls: []string{
				"blkid -o value -s TYPE /dev/sdb",
				"mkfs.xfs -f /dev/sdb",
				"mount -t xfs -o defaults /dev/sdb %s",
			},
		},
		{
			name:   "formatted device is fsck'd then mounted",
			fsType: "ext4",
			blkid:  "ext4\n",
			wantCalls: []string{
				"blkid -o value -s TYPE /dev/sdb",
				"fsck -a /dev/sdb",
				"mount -t ext4 -o defaults /dev/sdb %s",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			targetDir := filepath.Join(tmpDir, strings.ReplaceAll(tc.name, " ", "-"))
			fakeMounter := mountutils.NewFakeMounter(nil)
			fe := newFakeExec().withMountTable(fakeMounter)
			fe.handlers["blkid"] = func(_ ...string) ([]byte, error) {
				return []byte(tc.blkid), nil
			}
			fe.handlers["mkfs."+tc.fsType] = func(_ ...string) ([]byte, error) {
				return []byte("done"), nil
			}
			mounter := NewLinuxMounter(
				WithMountUtilsInterface(fakeMounter),
				WithExecInterface(fe),
			)

			if err := mounter.FormatAndMount(
				t.Context(),
				"/dev/sdb",
				targetDir,
				tc.fsType,
				nil,
			); err != nil {
				t.Fatalf("expected FormatAndMount success, got: %v", err)
			}

			want := make([]string, 0, len(tc.wantCalls))
			for _, call := range tc.wantCalls {
				want = append(want, strings.ReplaceAll(call, "%s", targetDir))
			}
			if got := fe.recordedCalls(); !slices.Equal(got, want) {
				t.Errorf("expected the host commands %v, got: %v", want, got)
			}
		})
	}
}

// Cancelled FormatAndMount leaves no mount; retry can finish stage.
func TestFormatAndMountRestagesAfterInterruption(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	stagingPath := filepath.Join(tmpDir, "staging")

	fakeMounter := mountutils.NewFakeMounter(nil)
	fe := newFakeExec().withMountTable(fakeMounter).trackStarts()
	fe.handlers["blkid"] = func(_ ...string) ([]byte, error) { return []byte(""), nil }
	fe.handlers["mkfs.ext4"] = func(_ ...string) ([]byte, error) { return []byte("done"), nil }
	fe.blocking["mkfs.ext4"] = true
	mounter := NewLinuxMounter(
		WithMountUtilsInterface(fakeMounter),
		WithExecInterface(fe),
	)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- mounter.FormatAndMount(ctx, "/dev/sdb", stagingPath, "ext4", nil)
	}()

	waitForCommand(t, fe, "mkfs.ext4")
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled for an interrupted stage, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled in-flight format did not return; the host command is unbounded")
	}
	if len(fakeMounter.MountPoints) != 0 {
		t.Fatalf("an interrupted stage left %d mounts behind", len(fakeMounter.MountPoints))
	}

	fe.blocking["mkfs.ext4"] = false
	if err := mounter.FormatAndMount(
		t.Context(),
		"/dev/sdb",
		stagingPath,
		"ext4",
		nil,
	); err != nil {
		t.Fatalf("expected the interrupted stage to be re-stagable, got: %v", err)
	}
	if len(fakeMounter.MountPoints) != 1 {
		t.Errorf(
			"expected the retry to mount the staging path, got %d mounts",
			len(fakeMounter.MountPoints),
		)
	}
}
