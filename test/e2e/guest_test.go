//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/report"
)

const (
	// byIDDir holds the stable links udev derives from a disk serial.
	byIDDir = "/dev/disk/by-id"
	// serialTruncation is the byte budget a virtio serial field offers,
	// which is shorter than a volume identifier, so udev writes a
	// truncated name.
	serialTruncation = 20

	toolUnavailable = "unavailable"
)

// collectGuestInfo reports what it can. A missing tool is recorded, not
// fatal: the scenario that needs one fails with a better message than
// this would.
func collectGuestInfo(ctx context.Context) report.GuestInfo {
	return report.GuestInfo{
		Kernel:   firstLine(runTool(ctx, "uname", "-r")),
		Udev:     firstLine(runTool(ctx, "udevadm", "--version")),
		Blkid:    firstLine(runTool(ctx, "blkid", "-V")),
		MkfsExt4: firstLine(runTool(ctx, "mkfs.ext4", "-V")),
		MkfsXfs:  firstLine(runTool(ctx, "mkfs.xfs", "-V")),
	}
}

func runTool(ctx context.Context, name string, args ...string) string {
	//nolint:gosec // the tool names are package constants, not input
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return toolUnavailable
	}
	return string(out)
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if line == "" {
		return toolUnavailable
	}
	return line
}

// serialCandidates returns the two spellings a link may carry for one
// volume: the complete identifier and the prefix a truncating serial
// field can hold.
func serialCandidates(volumeID string) []string {
	candidates := []string{volumeID}
	if len(volumeID) > serialTruncation {
		candidates = append(candidates, volumeID[:serialTruncation])
	}
	return candidates
}

// byIDLinksFor returns the stable links naming one volume, sorted for a
// stable evidence record.
func byIDLinksFor(volumeID string) ([]string, error) {
	entries, err := os.ReadDir(byIDDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", byIDDir, err)
	}

	var links []string
	for _, entry := range entries {
		for _, candidate := range serialCandidates(volumeID) {
			if strings.Contains(entry.Name(), candidate) {
				links = append(links, filepath.Join(byIDDir, entry.Name()))
				break
			}
		}
	}
	sort.Strings(links)
	return links, nil
}

// kernelDevice reports the device number a path resolves to, so two
// names can be compared without assuming either is canonical.
func kernelDevice(path string) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return stat.Rdev, nil
}

func sameKernelDevice(left, right string) (bool, error) {
	leftDevice, err := kernelDevice(left)
	if err != nil {
		return false, err
	}
	rightDevice, err := kernelDevice(right)
	if err != nil {
		return false, err
	}
	return leftDevice == rightDevice, nil
}

// isBlockDevice excludes character devices, so a bind-mounted raw target
// stays distinguishable.
func isBlockDevice(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	return info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0, nil
}

// mountPointsUnder reads the kernel mount table and returns the mount
// points beneath dir, deepest first, so a nested mount is released
// before the one carrying it.
func mountPointsUnder(dir string) ([]string, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	defer func() { _ = file.Close() }()

	prefix := strings.TrimSuffix(dir, "/") + "/"

	var points []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// The mount point is the fifth field of a mountinfo line.
		if len(fields) < 5 {
			continue
		}
		point := fields[4]
		if point == dir || strings.HasPrefix(point, prefix) {
			points = append(points, point)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mount table: %w", err)
	}

	sort.Slice(points, func(i, j int) bool { return len(points[i]) > len(points[j]) })
	return points, nil
}

// unmountAllUnder releases every mount a run left beneath dir. Teardown
// calls it before reclaiming volumes, because an attached volume with a
// live mount cannot be detached.
func unmountAllUnder(ctx context.Context, dir string) error {
	points, err := mountPointsUnder(dir)
	if err != nil {
		return err
	}

	var failures []string
	for _, point := range points {
		if err := ctx.Err(); err != nil {
			if len(failures) == 0 {
				return err
			}
			return errors.Join(
				err,
				fmt.Errorf("could not release: %s", strings.Join(failures, "; ")),
			)
		}
		//nolint:gosec // the path comes from the kernel mount table, not from input
		if out, err := exec.CommandContext(ctx, "umount", point).CombinedOutput(); err != nil {
			failures = append(
				failures,
				fmt.Sprintf("%s: %s", point, strings.TrimSpace(string(out))),
			)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("could not release: %s", strings.Join(failures, "; "))
	}
	return nil
}
