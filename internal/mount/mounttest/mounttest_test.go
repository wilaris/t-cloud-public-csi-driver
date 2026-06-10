package mounttest_test

import (
	"slices"
	"testing"

	mountutils "k8s.io/mount-utils"

	"wilaris.dev/t-cloud-public-csi-driver/internal/mount/mounttest"
)

// Remount with nothing mounted must fail and leave the table empty.
func TestRemountOfAnUnmountedTargetFails(t *testing.T) {
	t.Parallel()

	table := mountutils.NewFakeMounter(nil)
	commands := mounttest.Commands(table)

	out, err := commands["mount"]("-o", "bind,remount,ro", "/dev/sdb", "/mnt/target")
	if err == nil {
		t.Fatalf("expected the remount of an unmounted target to fail, got output %q", out)
	}
	if len(table.MountPoints) != 0 {
		t.Errorf("the failed remount mounted something: %+v", table.MountPoints)
	}
}

// Remount updates options in place; does not add a second mount.
func TestRemountReplacesTheOptionsOfAnExistingMount(t *testing.T) {
	t.Parallel()

	table := mountutils.NewFakeMounter([]mountutils.MountPoint{
		{Device: "/dev/sdb", Path: "/mnt/target", Opts: []string{"bind"}},
	})
	commands := mounttest.Commands(table)

	if _, err := commands["mount"]("-o", "bind,remount,ro", "/dev/sdb", "/mnt/target"); err != nil {
		t.Fatalf("expected the remount to succeed, got: %v", err)
	}
	if len(table.MountPoints) != 1 {
		t.Fatalf("expected the target to stay mounted once, got %+v", table.MountPoints)
	}
	if want := []string{"bind", "remount", "ro"}; !slices.Equal(table.MountPoints[0].Opts, want) {
		t.Errorf("expected the options %v, got %v", want, table.MountPoints[0].Opts)
	}
}

// mount/umount with too few args fail.
func TestCommandsRejectArgumentsTooShortToNameATarget(t *testing.T) {
	t.Parallel()

	commands := mounttest.Commands(mountutils.NewFakeMounter(nil))

	if _, err := commands["mount"]("/dev/sdb"); err == nil {
		t.Error("expected a mount without a target to fail")
	}
	if _, err := commands["umount"](); err == nil {
		t.Error("expected a umount without a target to fail")
	}
}

func TestParseMountArgs(t *testing.T) {
	t.Parallel()

	source, target, fsType, options := mounttest.ParseMountArgs(
		[]string{"-t", "ext4", "-o", "rw,discard", "/dev/sdb", "/mnt/target"},
	)
	if source != "/dev/sdb" || target != "/mnt/target" || fsType != "ext4" {
		t.Errorf("expected /dev/sdb at /mnt/target as ext4, got %s at %s as %s",
			source, target, fsType)
	}
	if want := []string{"rw", "discard"}; !slices.Equal(options, want) {
		t.Errorf("expected the options %v, got %v", want, options)
	}

	// Too few args: empty results, no panic.
	source, target, fsType, options = mounttest.ParseMountArgs([]string{"/dev/sdb"})
	if source != "" || target != "" || fsType != "" || options != nil {
		t.Errorf("expected an incomplete command line to name nothing, got %q %q %q %v",
			source, target, fsType, options)
	}
}
