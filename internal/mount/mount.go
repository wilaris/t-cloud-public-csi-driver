// Package mount implements host block device discovery, formatting, mounting and unmounting.
package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	mountutils "k8s.io/mount-utils"
	k8sexec "k8s.io/utils/exec"
)

var (
	// ErrInvalidInput indicates an invalid or missing argument.
	ErrInvalidInput = errors.New("invalid input")
	// ErrDeviceNotFound indicates the target block device could not be discovered.
	ErrDeviceNotFound = errors.New("device not found")
	// ErrDeviceIdentityUnverified: by-id and published path disagree.
	ErrDeviceIdentityUnverified = errors.New("device identity not verified")
	// ErrUnsupportedFilesystem indicates an unsupported filesystem requested for formatting/mounting.
	ErrUnsupportedFilesystem = errors.New("unsupported filesystem type")
	// ErrMountFailed indicates a failure during mount execution.
	ErrMountFailed = errors.New("mount operation failed")
	// ErrUnmountFailed indicates a failure during unmount execution.
	ErrUnmountFailed = errors.New("unmount operation failed")
	// ErrFilesystemMismatch indicates the device is formatted with a filesystem different from
	// requested.
	ErrFilesystemMismatch = errors.New("filesystem type mismatch")
	// ErrFilesystemFormatFailed indicates formatting a device failed.
	ErrFilesystemFormatFailed = errors.New("filesystem format failed")
	// ErrFilesystemDetectionFailed indicates the filesystem on a device could not be determined.
	ErrFilesystemDetectionFailed = errors.New("filesystem detection failed")
)

const (
	// DefaultFilesystemType is the filesystem used when a caller does not request one.
	DefaultFilesystemType = "ext4"
	// blkidNoFilesystem is the blkid exit status when a device has no filesystem
	// or the requested token was not found.
	blkidNoFilesystem = 2
	// virtioSerialMaxLen is the byte length of the VirtIO serial field. udev truncates
	// longer volume identifiers when it creates /dev/disk/by-id/virtio-* links.
	virtioSerialMaxLen = 20
	// mountPathPerm is the permission mode for driver-created mount directories.
	mountPathPerm = 0o750
	// fsckErrorsUncorrected is the fsck exit status when it found errors it could not correct.
	fsckErrorsUncorrected = 4
	// umountNotMounted is the lowercased umount output for a target that is already unmounted.
	umountNotMounted = "not mounted"
	// bindRollbackTimeout bounds the unmount that undoes a bind whose remount did not complete.
	bindRollbackTimeout = 30 * time.Second
)

// ST_* f_flags values from statfs. syscall does not export them. Not MS_* mount flags: most match,
// but relatime differs (MS_RELATIME is 0x200000).
const (
	statfsReadOnly    = 0x0001
	statfsNoSUID      = 0x0002
	statfsNoDev       = 0x0004
	statfsNoExec      = 0x0008
	statfsNoATime     = 0x0400
	statfsNoDirATime  = 0x0800
	statfsRelATime    = 0x1000
	statfsNoSymFollow = 0x2000
)

// bindRemountOptions maps source mount flags a bind remount must restate.
// MS_BIND|MS_REMOUNT sets VFS flags to exactly the flags passed, so source ro/nosuid/etc. are lost
// at the target unless listed again. Order is fixed for a stable command line.
//
// Only per-mount flags from statfs. Superblock flags (sync, mandlock) are shared with the source;
// remount cannot change them.
var bindRemountOptions = []struct {
	flag   int64
	option string
}{
	{statfsReadOnly, "ro"},
	{statfsNoSUID, "nosuid"},
	{statfsNoDev, "nodev"},
	{statfsNoExec, "noexec"},
	{statfsNoATime, "noatime"},
	{statfsNoDirATime, "nodiratime"},
	{statfsRelATime, "relatime"},
	{statfsNoSymFollow, "nosymfollow"},
}

// IsSupportedFilesystemType is true for ext4 and xfs (case-insensitive).
func IsSupportedFilesystemType(fsType string) bool {
	switch strings.ToLower(fsType) {
	case "ext4", "xfs":
		return true
	default:
		return false
	}
}

// Mounter defines host block device discovery, formatting, mounting and unmounting operations.
type Mounter interface {
	DiscoverDevice(ctx context.Context, volumeID, publishedDevicePath string) (string, error)
	FormatAndMount(
		ctx context.Context,
		source string,
		target string,
		fsType string,
		options []string,
	) error
	Mount(
		ctx context.Context,
		source string,
		target string,
		fsType string,
		options []string,
	) error
	Unmount(ctx context.Context, target string) error
	IsMountPoint(ctx context.Context, target string) (bool, error)
	GetFilesystemType(ctx context.Context, source string) (string, error)
	GetMountSource(ctx context.Context, target string) (string, error)
}

// LinuxMounter implements Mounter. It uses k8s.io/mount-utils for mount table parsing and
// verification, and runs every blocking host command through k8s.io/utils/exec with the caller's
// context. Cancellation and deadlines kill the in-flight subprocess and are reported as the
// context cause. A command stuck in an uninterruptible kernel wait still blocks until the kernel
// releases it.
type LinuxMounter struct {
	mounter     mountutils.Interface
	exec        k8sexec.Interface
	statfs      func(path string, buf *syscall.Statfs_t) error
	devDir      string
	diskByIDDir string
}

// MounterOption configures a LinuxMounter.
type MounterOption func(*LinuxMounter)

// WithMountUtilsInterface overrides the mountutils.Interface used for mount table parsing.
func WithMountUtilsInterface(mounter mountutils.Interface) MounterOption {
	return func(m *LinuxMounter) {
		m.mounter = mounter
	}
}

// WithExecInterface overrides the k8sexec.Interface used to run host commands.
func WithExecInterface(exec k8sexec.Interface) MounterOption {
	return func(m *LinuxMounter) {
		m.exec = exec
	}
}

// WithDiskByIDDir overrides the default /dev/disk/by-id directory.
func WithDiskByIDDir(dir string) MounterOption {
	return func(m *LinuxMounter) {
		m.diskByIDDir = dir
	}
}

// WithDevDir overrides the default /dev directory that a published device path must stay inside.
func WithDevDir(dir string) MounterOption {
	return func(m *LinuxMounter) {
		m.devDir = dir
	}
}

// WithStatfsFunc overrides statfs used to read source mount flags for bind remount. Tests use this
// so remount options do not depend on the host's real mounts.
func WithStatfsFunc(statfs func(path string, buf *syscall.Statfs_t) error) MounterOption {
	return func(m *LinuxMounter) {
		m.statfs = statfs
	}
}

// NewLinuxMounter creates a new LinuxMounter instance.
func NewLinuxMounter(opts ...MounterOption) *LinuxMounter {
	m := &LinuxMounter{
		mounter:     mountutils.New(""),
		exec:        k8sexec.New(),
		statfs:      syscall.Statfs,
		devDir:      "/dev",
		diskByIDDir: "/dev/disk/by-id",
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// command runs name with LC_ALL=C. Some checks match command output, which is locale-dependent.
func (m *LinuxMounter) command(ctx context.Context, name string, args ...string) k8sexec.Cmd {
	cmd := m.exec.CommandContext(ctx, name, args...)
	cmd.SetEnv(append(os.Environ(), "LC_ALL=C"))
	return cmd
}

// DiscoverDevice resolves the block device for volumeID on this node.
// publishedDevicePath is the attach-time path from the cloud. A by-id link for the volume ID
// and that path must resolve to the same kernel device (virtio serials may be truncated).
func (m *LinuxMounter) DiscoverDevice(
	ctx context.Context,
	volumeID, publishedDevicePath string,
) (string, error) {
	if volumeID == "" {
		return "", fmt.Errorf("%w: volume identifier is empty", ErrInvalidInput)
	}
	if publishedDevicePath == "" {
		return "", fmt.Errorf("%w: published device path is empty", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("discover device for volume %s: %w", volumeID, err)
	}
	if !m.holdsDevice(publishedDevicePath) {
		return "", fmt.Errorf(
			"%w: published device path %s is not a device in %s",
			ErrInvalidInput,
			publishedDevicePath,
			m.devDir,
		)
	}

	if _, err := os.Stat(publishedDevicePath); err != nil {
		return "", fmt.Errorf(
			"%w: published device path %s is not present on this node",
			ErrDeviceNotFound,
			publishedDevicePath,
		)
	}
	published := resolveSymlink(publishedDevicePath)

	links := m.serialLinks(volumeID)
	if len(links) == 0 {
		return "", fmt.Errorf(
			"%w: no %s link identifies volume %s",
			ErrDeviceNotFound,
			m.diskByIDDir,
			volumeID,
		)
	}

	// All volume serial links must resolve to the published device.
	for _, link := range links {
		if resolveSymlink(link) != published {
			return "", fmt.Errorf(
				"%w: %s and published device path %s name different devices",
				ErrDeviceIdentityUnverified,
				link,
				publishedDevicePath,
			)
		}
	}

	return published, nil
}

// holdsDevice is true for absolute paths under the node device dir.
func (m *LinuxMounter) holdsDevice(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}

	rel, err := filepath.Rel(m.devDir, filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// serialLinks returns existing by-id links for volumeID: full serial and the VirtIO-truncated form
// when the ID is longer than virtioSerialMaxLen.
func (m *LinuxMounter) serialLinks(volumeID string) []string {
	serials := []string{volumeID}
	if len(volumeID) > virtioSerialMaxLen {
		serials = append(serials, volumeID[:virtioSerialMaxLen])
	}

	links := make([]string, 0, len(serials))
	for _, serial := range serials {
		for _, name := range []string{
			serial,
			"virtio-" + serial,
			"nvme-" + serial,
			"scsi-" + serial,
		} {
			link := filepath.Join(m.diskByIDDir, name)
			if _, err := os.Stat(link); err == nil {
				links = append(links, link)
			}
		}
	}

	return links
}

// GetFilesystemType inspects the filesystem on source via blkid or lsblk.
// Empty string + nil err means unformatted; detection failure is an error. Cancellation is
// reported as the context cause and stops the fallback probe from starting.
func (m *LinuxMounter) GetFilesystemType(ctx context.Context, source string) (string, error) {
	if source == "" {
		return "", fmt.Errorf("%w: source path is empty", ErrInvalidInput)
	}

	cmd := m.command(ctx, "blkid", "-o", "value", "-s", "TYPE", source)
	out, blkidErr := cmd.CombinedOutput()
	if blkidErr == nil {
		return strings.TrimSpace(string(out)), nil
	}

	var exitErr k8sexec.ExitError
	if errors.As(blkidErr, &exitErr) && exitErr.ExitStatus() == blkidNoFilesystem {
		return "", nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("detect filesystem on %s: %w", source, ctxErr)
	}

	lsblkCmd := m.command(ctx, "lsblk", "-no", "FSTYPE", source)
	lsblkOut, lsblkErr := lsblkCmd.CombinedOutput()
	if lsblkErr == nil {
		return strings.TrimSpace(string(lsblkOut)), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("detect filesystem on %s: %w", source, ctxErr)
	}

	return "", fmt.Errorf(
		"%w: blkid on %s failed (%v) and the lsblk fallback failed (%v)",
		ErrFilesystemDetectionFailed,
		source,
		blkidErr,
		lsblkErr,
	)
}

// FormatAndMount formats if needed then mounts a block device (not for bind/raw publish).
// Detection, formatting, fsck and mounting run under the caller's context; cancellation after a
// command has started kills it and is reported as the context cause. An interrupted call leaves the
// target unmounted so the caller can stage again.
func (m *LinuxMounter) FormatAndMount(
	ctx context.Context,
	source string,
	target string,
	fsType string,
	options []string,
) error {
	if source == "" {
		return fmt.Errorf("%w: source path is required", ErrInvalidInput)
	}
	if target == "" {
		return fmt.Errorf("%w: target path is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("format and mount %s at %s: %w", source, target, err)
	}

	if fsType == "" {
		fsType = DefaultFilesystemType
	}
	fsType = strings.ToLower(fsType)
	if !IsSupportedFilesystemType(fsType) {
		return fmt.Errorf("%w: %s (supported: ext4, xfs)", ErrUnsupportedFilesystem, fsType)
	}

	if _, err := os.Stat(target); os.IsNotExist(err) {
		if err := os.MkdirAll(target, mountPathPerm); err != nil {
			return fmt.Errorf("failed to create target directory %s: %w", target, err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to stat target path %s: %w", target, err)
	}

	mounted, err := m.IsMountPoint(ctx, target)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	existingFSType, err := m.GetFilesystemType(ctx, source)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem on %s: %w", source, err)
	}

	if existingFSType != "" && existingFSType != fsType {
		return fmt.Errorf(
			"%w: device %s is formatted as %s, requested %s",
			ErrFilesystemMismatch,
			source,
			existingFSType,
			fsType,
		)
	}

	readOnly := slices.Contains(options, "ro")
	if existingFSType == "" {
		if readOnly {
			return fmt.Errorf(
				"%w: cannot mount unformatted device %s read-only",
				ErrMountFailed,
				source,
			)
		}
		if err := m.format(ctx, source, fsType); err != nil {
			return err
		}
	} else if !readOnly {
		if err := m.checkFilesystem(ctx, source); err != nil {
			return err
		}
	}

	return m.mount(ctx, source, target, fsType, append(slices.Clone(options), "defaults"))
}

// format runs mkfs.<fsType> with the same args SafeFormatAndMount used. ext4 and xfs keep separate
// arg lists.
func (m *LinuxMounter) format(ctx context.Context, source, fsType string) error {
	var args []string
	switch fsType {
	case "ext4":
		args = []string{"-F", "-m0", source}
	case "xfs":
		args = []string{"-f", source}
	default:
		return fmt.Errorf("%w: %s (supported: ext4, xfs)", ErrUnsupportedFilesystem, fsType)
	}

	out, err := m.command(ctx, "mkfs."+fsType, args...).CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("format %s as %s: %w", source, fsType, ctxErr)
		}
		return fmt.Errorf(
			"%w: failed to format %s as %s: %v: %s",
			ErrFilesystemFormatFailed,
			source,
			fsType,
			err,
			strings.TrimSpace(string(out)),
		)
	}

	return nil
}

// checkFilesystem runs fsck -a on an already formatted device before read-write mount. Only
// uncorrectable fsck errors fail the mount; a missing fsck binary and other outcomes are tolerated,
// matching SafeFormatAndMount. Cancellation is the context cause.
func (m *LinuxMounter) checkFilesystem(ctx context.Context, source string) error {
	out, err := m.command(ctx, "fsck", "-a", source).CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("check filesystem on %s: %w", source, ctxErr)
		}
		var exitErr k8sexec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitStatus() == fsckErrorsUncorrected {
			return fmt.Errorf(
				"%w: fsck found errors on %s it could not correct: %s",
				ErrMountFailed,
				source,
				strings.TrimSpace(string(out)),
			)
		}
	}

	return nil
}

// mount performs the requested mount with the same command sequence the mount-utils library used.
// A bind mount takes the two-step path below; every other mount is a single command.
func (m *LinuxMounter) mount(
	ctx context.Context,
	source, target, fsType string,
	options []string,
) error {
	if flavor := bindFlavor(options); flavor != "" {
		return m.bindMount(ctx, source, target, fsType, flavor, options)
	}

	return m.runMount(ctx, source, target, fsType, options)
}

// bindFlavor returns "bind" or "rbind" if present in options, else "". Both need the two-step bind
// path: the kernel ignores other flags with MS_BIND.
func bindFlavor(options []string) string {
	for _, option := range options {
		switch option {
		case "bind", "rbind":
			return option
		}
	}

	return ""
}

// bindMount binds source at target and then remounts to apply the requested options. The kernel
// ignores every other flag passed alongside MS_BIND, so the options only take effect on the second
// call.
func (m *LinuxMounter) bindMount(
	ctx context.Context,
	source, target, fsType string,
	flavor string,
	options []string,
) error {
	// _netdev is a userspace option and is not inherited by the bind, so it is carried over.
	bindOptions := []string{flavor}
	if slices.Contains(options, "_netdev") {
		bindOptions = append(bindOptions, "_netdev")
	}
	if err := m.runMount(ctx, source, target, fsType, bindOptions); err != nil {
		return err
	}

	if err := m.remountBind(ctx, source, target, fsType, options); err != nil {
		// Remount failed: bind is up with source flags only. Unmount so a retry does not treat the
		// target as already published.
		m.rollBackBind(ctx, target)
		return err
	}

	return nil
}

// remountBind applies options on an existing bind (second step of bind mount). Restates source
// flags so MS_BIND|MS_REMOUNT does not drop them. Idempotent: also finishes a bind interrupted
// before remount. Always uses plain "bind" on remount, even after rbind; only the named mount gets
// new flags.
func (m *LinuxMounter) remountBind(
	ctx context.Context,
	source, target, fsType string,
	options []string,
) error {
	sourceOptions, err := m.sourceMountOptions(source)
	if err != nil {
		return fmt.Errorf(
			"%w: failed to read the mount flags of %s: %v",
			ErrMountFailed,
			source,
			err,
		)
	}

	// mount applies the last of conflicting options: list source flags first so the caller's
	// options win. Do not honor "rw" if the source is read-only.
	sourceReadOnly := slices.Contains(sourceOptions, "ro")
	remountOptions := []string{"bind", "remount"}
	for _, option := range slices.Concat(sourceOptions, options) {
		switch option {
		case "bind", "rbind", "remount":
			continue
		case "rw":
			if sourceReadOnly {
				continue
			}
		}
		if !slices.Contains(remountOptions, option) {
			remountOptions = append(remountOptions, option)
		}
	}

	return m.runMount(ctx, source, target, fsType, remountOptions)
}

// rollBackBind unmounts a bind after a failed remount. Uses a context detached from the caller so
// cancellation cannot skip cleanup. Error is ignored; the caller already has the remount error.
func (m *LinuxMounter) rollBackBind(ctx context.Context, target string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindRollbackTimeout)
	defer cancel()

	_ = m.Unmount(ctx, target)
}

// sourceMountOptions returns mount options for source flags a bind remount would clear.
func (m *LinuxMounter) sourceMountOptions(source string) ([]string, error) {
	var stat syscall.Statfs_t
	if err := m.statfs(source, &stat); err != nil {
		return nil, err
	}

	options := make([]string, 0, len(bindRemountOptions))
	for _, mapping := range bindRemountOptions {
		if int64(stat.Flags)&mapping.flag == mapping.flag {
			options = append(options, mapping.option)
		}
	}

	return options, nil
}

// runMount runs one mount command under the caller's context with the same argument shape the
// mount-utils library used. Cancellation kills the in-flight command and is reported as the
// context cause, not ErrMountFailed.
func (m *LinuxMounter) runMount(
	ctx context.Context,
	source, target, fsType string,
	options []string,
) error {
	args := make([]string, 0, 6)
	if fsType != "" {
		args = append(args, "-t", fsType)
	}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, source, target)

	out, err := m.command(ctx, "mount", args...).CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("mount %s at %s: %w", source, target, ctxErr)
		}
		return fmt.Errorf(
			"%w: failed to mount %s at %s: %v: %s",
			ErrMountFailed,
			source,
			target,
			err,
			strings.TrimSpace(string(out)),
		)
	}

	return nil
}

// Mount does not inspect or format source (bind mounts, raw block). target must already exist. An
// already-mounted target succeeds; for a bind the remount is repeated so a publish interrupted
// between bind and remount still gets the right options.
func (m *LinuxMounter) Mount(
	ctx context.Context,
	source string,
	target string,
	fsType string,
	options []string,
) error {
	if source == "" {
		return fmt.Errorf("%w: source path is required", ErrInvalidInput)
	}
	if target == "" {
		return fmt.Errorf("%w: target path is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mount %s at %s: %w", source, target, err)
	}

	mounted, err := m.IsMountPoint(ctx, target)
	if err != nil {
		return err
	}
	if mounted {
		// Already mounted is not enough for binds: a missing remount still shows as mounted.
		// Re-run remount so options match the request.
		if bindFlavor(options) != "" {
			return m.remountBind(ctx, source, target, fsType, options)
		}
		return nil
	}

	return m.mount(ctx, source, target, fsType, options)
}

// Unmount unmounts target path idempotently.
func (m *LinuxMounter) Unmount(ctx context.Context, target string) error {
	if target == "" {
		return fmt.Errorf("%w: target path is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}

	mounted, err := m.IsMountPoint(ctx, target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}

	out, err := m.command(ctx, "umount", target).CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("unmount %s: %w", target, ctxErr)
		}
		// The mount point check above can lose a race with a concurrent unmount. umount then
		// reports the target as already unmounted, which is the outcome this call wanted.
		if strings.Contains(strings.ToLower(string(out)), umountNotMounted) {
			return nil
		}
		return fmt.Errorf(
			"%w: failed to unmount %s: %v: %s",
			ErrUnmountFailed,
			target,
			err,
			strings.TrimSpace(string(out)),
		)
	}

	return nil
}

// IsMountPoint reports whether target is a mount point. Uses the mount table so bind mounts are
// detected. Missing path is not mounted; corrupted mounts report as mounted so callers can unmount
// them.
func (m *LinuxMounter) IsMountPoint(ctx context.Context, target string) (bool, error) {
	if target == "" {
		return false, fmt.Errorf("%w: target path is empty", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("check mount point %s: %w", target, err)
	}

	mounted, err := m.mounter.IsMountPoint(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if mountutils.IsCorruptedMnt(err) {
			return true, nil
		}
		return false, fmt.Errorf("failed to check mount point %s: %w", target, err)
	}

	return mounted, nil
}

// GetMountSource returns the device for target from the mount table. Checks ctx first; the table
// read itself is not context-aware.
func (m *LinuxMounter) GetMountSource(ctx context.Context, target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("%w: target path is empty", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("get mount source for %s: %w", target, err)
	}

	devName, _, err := mountutils.GetDeviceNameFromMount(m.mounter, target)
	if err != nil {
		return "", fmt.Errorf("failed to get device name for %s: %w", target, err)
	}
	if devName == "" {
		return "", fmt.Errorf("mount source not found for target %s", target)
	}

	return devName, nil
}

func resolveSymlink(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}
	return path
}
