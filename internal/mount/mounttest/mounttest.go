// Package mounttest provides fake mount/umount commands that keep a FakeMounter in sync when tests
// exercise the real exec boundary.
package mounttest

import (
	"fmt"
	"slices"
	"strings"
	"syscall"

	mountutils "k8s.io/mount-utils"
)

// mountBadOption is the exit status mount(8) reports for a request the kernel rejected.
const mountBadOption = 32

// mountExitError is a mount or umount failure carrying the exit status of the real command.
type mountExitError struct {
	code int
}

func (e mountExitError) String() string  { return fmt.Sprintf("exit status %d", e.code) }
func (e mountExitError) Error() string   { return e.String() }
func (e mountExitError) Exited() bool    { return true }
func (e mountExitError) ExitStatus() int { return e.code }

// Commands returns mount/umount fakes that update table (exec path, not Interface-only).
func Commands(table *mountutils.FakeMounter) map[string]func(args ...string) ([]byte, error) {
	return map[string]func(args ...string) ([]byte, error){
		"mount": func(args ...string) ([]byte, error) {
			if len(args) < 2 {
				return []byte("mount: bad usage"), mountExitError{code: mountBadOption}
			}
			source, target, fsType, options := ParseMountArgs(args)
			// Remount updates options in place; does not stack a second mount.
			if slices.Contains(options, "remount") {
				for i, mountPoint := range table.MountPoints {
					if mountPoint.Path == target {
						table.MountPoints[i].Opts = options
						return nil, nil
					}
				}
				// No mount at target: fail like the kernel. Do not create one (would hide a
				// half-done bind).
				return []byte("mount: " + target + ": mount point not mounted or bad option."),
					mountExitError{code: mountBadOption}
			}
			return nil, table.Mount(source, target, fsType, options)
		},
		"umount": func(args ...string) ([]byte, error) {
			if len(args) == 0 {
				return []byte("umount: bad usage"), mountExitError{code: mountBadOption}
			}
			return nil, table.Unmount(args[len(args)-1])
		},
	}
}

// ParseMountArgs splits mount argv into source, target, fs type and options. Source and target are
// the last two args. Shorter input returns zeros.
func ParseMountArgs(args []string) (source, target, fsType string, options []string) {
	if len(args) < 2 {
		return "", "", "", nil
	}

	source, target = args[len(args)-2], args[len(args)-1]
	for i := 0; i < len(args)-2; i++ {
		switch args[i] {
		case "-t":
			fsType = args[i+1]
			i++
		case "-o":
			options = strings.Split(args[i+1], ",")
			i++
		}
	}

	return source, target, fsType, options
}

// StaticStatfs returns fixed flags for any path so bind remount tests are host-independent.
func StaticStatfs(flags int64) func(string, *syscall.Statfs_t) error {
	return func(_ string, buf *syscall.Statfs_t) error {
		buf.Flags = flags
		return nil
	}
}
