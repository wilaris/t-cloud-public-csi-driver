// Package mount implements host block device discovery, formatting, mounting, and unmounting.
package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mountutils "k8s.io/mount-utils"
	k8sexec "k8s.io/utils/exec"
)

var (
	// ErrInvalidInput indicates an invalid or missing argument.
	ErrInvalidInput = errors.New("invalid input")
	// ErrDeviceNotFound indicates the target block device could not be discovered.
	ErrDeviceNotFound = errors.New("device not found")
	// ErrUnsupportedFilesystem indicates an unsupported filesystem requested for formatting/mounting.
	ErrUnsupportedFilesystem = errors.New("unsupported filesystem type")
	// ErrMountFailed indicates a failure during mount execution.
	ErrMountFailed = errors.New("mount operation failed")
	// ErrUnmountFailed indicates a failure during unmount execution.
	ErrUnmountFailed = errors.New("unmount operation failed")
	// ErrFilesystemMismatch indicates the device is formatted with a filesystem different from requested.
	ErrFilesystemMismatch = errors.New("filesystem type mismatch")
	// ErrFilesystemFormatFailed indicates formatting a device failed.
	ErrFilesystemFormatFailed = errors.New("filesystem format failed")
	// ErrFilesystemDetectionFailed indicates the filesystem on a device could not be determined.
	ErrFilesystemDetectionFailed = errors.New("filesystem detection failed")
)

const (
	// DefaultFilesystemType is the filesystem used when a caller does not request one.
	DefaultFilesystemType = "ext4"
	// blkidNoFilesystem is the blkid exit status reported when a device carries no
	// filesystem, or when the requested token was not found.
	blkidNoFilesystem = 2
	// virtioSerialMaxLen is the byte length of the VirtIO serial field. udev truncates
	// longer volume identifiers when it creates /dev/disk/by-id/virtio-* links.
	virtioSerialMaxLen = 20
	// mountPathPerm is the permission mode for driver-created mount directories.
	mountPathPerm = 0o750
)

// Mounter defines host block device discovery, formatting, mounting, and unmounting operations.
type Mounter interface {
	DiscoverDevice(ctx context.Context, deviceIdentifier string) (string, error)
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

// LinuxMounter implements Mounter using k8s.io/mount-utils and k8s.io/utils/exec.
type LinuxMounter struct {
	mounter     mountutils.Interface
	exec        k8sexec.Interface
	safeMounter *mountutils.SafeFormatAndMount
	diskByIDDir string
}

// MounterOption configures a LinuxMounter.
type MounterOption func(*LinuxMounter)

// WithMountUtilsInterface overrides the mountutils.Interface.
func WithMountUtilsInterface(mounter mountutils.Interface) MounterOption {
	return func(m *LinuxMounter) {
		m.mounter = mounter
		m.safeMounter.Interface = mounter
	}
}

// WithExecInterface overrides the k8sexec.Interface.
func WithExecInterface(exec k8sexec.Interface) MounterOption {
	return func(m *LinuxMounter) {
		m.exec = exec
		m.safeMounter.Exec = exec
	}
}

// WithDiskByIDDir overrides the default /dev/disk/by-id directory.
func WithDiskByIDDir(dir string) MounterOption {
	return func(m *LinuxMounter) {
		m.diskByIDDir = dir
	}
}

// NewLinuxMounter creates a new LinuxMounter instance backed by k8s.io/mount-utils.
func NewLinuxMounter(opts ...MounterOption) *LinuxMounter {
	mounter := mountutils.New("")
	exec := k8sexec.New()
	safeMounter := &mountutils.SafeFormatAndMount{
		Interface: mounter,
		Exec:      exec,
	}

	m := &LinuxMounter{
		mounter:     mounter,
		exec:        exec,
		safeMounter: safeMounter,
		diskByIDDir: "/dev/disk/by-id",
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// DiscoverDevice resolves a volume ID or device name to an absolute block device path.
func (m *LinuxMounter) DiscoverDevice(
	ctx context.Context,
	deviceIdentifier string,
) (string, error) {
	if deviceIdentifier == "" {
		return "", fmt.Errorf("%w: device identifier is empty", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("discover device %s: %w", deviceIdentifier, err)
	}

	// If already absolute path, verify existence
	if strings.HasPrefix(deviceIdentifier, "/") {
		if _, err := os.Stat(deviceIdentifier); err == nil {
			return resolveSymlink(deviceIdentifier), nil
		}
	}

	for _, cand := range m.deviceCandidates(deviceIdentifier) {
		if _, err := os.Stat(cand); err == nil {
			return resolveSymlink(cand), nil
		}
	}

	return "", fmt.Errorf(
		"%w: volume %s not found in %s or /dev",
		ErrDeviceNotFound,
		deviceIdentifier,
		m.diskByIDDir,
	)
}

// deviceCandidates returns the paths that may expose deviceIdentifier, most specific first.
//
// A VirtIO serial holds at most virtioSerialMaxLen bytes, so udev truncates a 36-character
// EVS volume UUID when it creates the /dev/disk/by-id/virtio-<serial> link. The truncated
// identifier is therefore tried alongside the full one.
func (m *LinuxMounter) deviceCandidates(deviceIdentifier string) []string {
	identifiers := []string{deviceIdentifier}
	if len(deviceIdentifier) > virtioSerialMaxLen {
		identifiers = append(identifiers, deviceIdentifier[:virtioSerialMaxLen])
	}

	candidates := make([]string, 0, len(identifiers)*4+1)
	for _, identifier := range identifiers {
		candidates = append(
			candidates,
			filepath.Join(m.diskByIDDir, identifier),
			filepath.Join(m.diskByIDDir, "virtio-"+identifier),
			filepath.Join(m.diskByIDDir, "nvme-"+identifier),
			filepath.Join(m.diskByIDDir, "scsi-"+identifier),
		)
	}

	// A stable by-id link is preferred over a kernel device name, so /dev is checked last.
	return append(candidates, filepath.Join("/dev", deviceIdentifier))
}

// GetFilesystemType inspects existing filesystem on source block device using blkid or lsblk.
//
// An empty string with a nil error means the device carries no filesystem. A device whose
// filesystem cannot be determined is reported as an error rather than as an empty string,
// so that callers never mistake an unreadable device for an unformatted one and overwrite it.
func (m *LinuxMounter) GetFilesystemType(ctx context.Context, source string) (string, error) {
	if source == "" {
		return "", fmt.Errorf("%w: source path is empty", ErrInvalidInput)
	}

	cmd := m.exec.CommandContext(ctx, "blkid", "-o", "value", "-s", "TYPE", source)
	out, blkidErr := cmd.CombinedOutput()
	if blkidErr == nil {
		return strings.TrimSpace(string(out)), nil
	}

	var exitErr k8sexec.ExitError
	if errors.As(blkidErr, &exitErr) && exitErr.ExitStatus() == blkidNoFilesystem {
		return "", nil
	}

	lsblkCmd := m.exec.CommandContext(ctx, "lsblk", "-no", "FSTYPE", source)
	lsblkOut, lsblkErr := lsblkCmd.CombinedOutput()
	if lsblkErr == nil {
		return strings.TrimSpace(string(lsblkOut)), nil
	}

	return "", fmt.Errorf(
		"%w: blkid on %s failed (%v) and the lsblk fallback failed (%v)",
		ErrFilesystemDetectionFailed,
		source,
		blkidErr,
		lsblkErr,
	)
}

// FormatAndMount formats an unformatted block device and mounts it to target.
//
// source must be a block device. It must not be used to bind mount an already mounted
// directory or to publish a raw block volume, because an unformatted source is formatted;
// use Mount for those.
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
	if fsType != "ext4" && fsType != "xfs" {
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

	if err := m.safeMounter.FormatAndMount(source, target, fsType, options); err != nil {
		return fmt.Errorf(
			"%w: failed to format/mount %s at %s: %v",
			ErrMountFailed,
			source,
			target,
			err,
		)
	}

	return nil
}

// Mount attaches source at target without inspecting or altering source.
//
// This is the operation required for bind mounts, where source is either an already staged
// directory or a raw block device that must be published exactly as it is. target must
// already exist, and must be a directory for a filesystem mount or a file for a raw block
// device. Mounting an already mounted target succeeds without remounting.
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
		return nil
	}

	if err := m.mounter.Mount(source, target, fsType, options); err != nil {
		return fmt.Errorf("%w: failed to mount %s at %s: %v", ErrMountFailed, source, target, err)
	}

	return nil
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

	if err := m.mounter.Unmount(target); err != nil {
		return fmt.Errorf("%w: failed to unmount %s: %v", ErrUnmountFailed, target, err)
	}

	return nil
}

// IsMountPoint checks if target path is a mount point.
//
// It consults the kernel mount table rather than comparing device numbers, so bind mounts
// and mounts that share a device with their parent directory are reported correctly. A path
// that does not exist is not a mount point. A corrupted mount is reported as mounted, so
// that callers can still unmount it.
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

// GetMountSource retrieves the device source path for a target mount point.
func (m *LinuxMounter) GetMountSource(ctx context.Context, target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("%w: target path is empty", ErrInvalidInput)
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
