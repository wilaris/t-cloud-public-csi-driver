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
	// ErrDeviceIdentityUnverified: by-id and published path disagree.
	ErrDeviceIdentityUnverified = errors.New("device identity not verified")
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
	// blkidNoFilesystem is the blkid exit status when a device has no filesystem
	// or the requested token was not found.
	blkidNoFilesystem = 2
	// virtioSerialMaxLen is the byte length of the VirtIO serial field. udev truncates
	// longer volume identifiers when it creates /dev/disk/by-id/virtio-* links.
	virtioSerialMaxLen = 20
	// mountPathPerm is the permission mode for driver-created mount directories.
	mountPathPerm = 0o750
)

// IsSupportedFilesystemType is true for ext4 and xfs (case-insensitive).
func IsSupportedFilesystemType(fsType string) bool {
	switch strings.ToLower(fsType) {
	case "ext4", "xfs":
		return true
	default:
		return false
	}
}

// Mounter defines host block device discovery, formatting, mounting, and unmounting operations.
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

// LinuxMounter implements Mounter using k8s.io/mount-utils and k8s.io/utils/exec.
type LinuxMounter struct {
	mounter     mountutils.Interface
	exec        k8sexec.Interface
	safeMounter *mountutils.SafeFormatAndMount
	devDir      string
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

// WithDevDir overrides the default /dev directory that a published device path must stay inside.
func WithDevDir(dir string) MounterOption {
	return func(m *LinuxMounter) {
		m.devDir = dir
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
		devDir:      "/dev",
		diskByIDDir: "/dev/disk/by-id",
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
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

// serialLinks returns the existing by-id links that claim volumeID, by its complete serial and by
// the serial udev truncates to the VirtIO serial field length.
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
// Empty string + nil err means unformatted; detection failure is an error.
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

// FormatAndMount formats if needed then mounts a block device (not for bind/raw publish).
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

// Mount does not inspect or format source (bind mounts, raw block).
// target must already exist. Already-mounted target succeeds without remounting.
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

// IsMountPoint checks if target is a mount point.
// Uses the mount table so bind mounts are detected correctly. Missing path is not mounted;
// corrupted mounts report as mounted so callers can unmount them.
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
