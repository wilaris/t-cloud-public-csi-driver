package driver_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

// devicePathContextKey is the publish context key for the attached device path.
const devicePathContextKey = "devicePath"

// reportedDeviceEVSClient returns a fixed device name from AttachVolume and records call args.
type reportedDeviceEVSClient struct {
	*mockEVSClient

	deviceName  string
	gotVolumeID string
	gotServerID string
	attachCalls int
}

func (c *reportedDeviceEVSClient) AttachVolume(
	_ context.Context,
	volumeID, serverID string,
) (*evs.Attachment, error) {
	c.attachCalls++
	c.gotVolumeID = volumeID
	c.gotServerID = serverID

	return &evs.Attachment{
		VolumeID:   volumeID,
		ServerID:   serverID,
		DeviceName: c.deviceName,
	}, nil
}

func TestControllerPublishVolume_PublishesOnlyTheReportedDevicePath(t *testing.T) {
	t.Parallel()

	const volumeID = "6f1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
	const serverUUID = "9f0a1b2c-3d4e-5f60-7182-93a4b5c6d7e8"

	tests := []struct {
		name     string
		reported string
		wantCode codes.Code
	}{
		{
			name:     "publishes the device name the cloud reported",
			reported: "/dev/vdb",
			wantCode: codes.OK,
		},
		{
			name:     "empty device name",
			reported: "",
			wantCode: codes.Internal,
		},
		{
			name:     "relative device name",
			reported: "vdb",
			wantCode: codes.Internal,
		},
		{
			// Cleaned path escapes /dev; not a device node under the device directory.
			name:     "unclean device name",
			reported: "/dev/../etc/shadow",
			wantCode: codes.Internal,
		},
		{
			name:     "trailing separator",
			reported: "/dev/vdb/",
			wantCode: codes.Internal,
		},
		{
			name:     "outside device directory",
			reported: "/tmp/vdb",
			wantCode: codes.Internal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cloud := &reportedDeviceEVSClient{
				mockEVSClient: newMockEVSClient(),
				deviceName:    tt.reported,
			}
			svc, err := driver.NewControllerService(cloud, validTestConfig())
			if err != nil {
				t.Fatalf("NewControllerService failed: %v", err)
			}
			client := newControllerClient(t, svc)

			resp, err := client.ControllerPublishVolume(
				t.Context(),
				&csi.ControllerPublishVolumeRequest{
					VolumeId: volumeID,
					NodeId:   serverUUID,
					VolumeCapability: accessModeCapability(
						csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					),
				},
			)
			if status.Code(err) != tt.wantCode {
				t.Fatalf("expected status %v, got %v", tt.wantCode, err)
			}

			// Attach is always attempted; only the reported name changes per case.
			if cloud.attachCalls != 1 {
				t.Errorf("expected exactly one attachment attempt, got %d", cloud.attachCalls)
			}
			if cloud.gotVolumeID != volumeID {
				t.Errorf(
					"expected the requested volume %q attached, got %q",
					volumeID,
					cloud.gotVolumeID,
				)
			}
			if cloud.gotServerID != serverUUID {
				t.Errorf(
					"expected Server UUID %q, got %q",
					serverUUID,
					cloud.gotServerID,
				)
			}

			publishContext := resp.GetPublishContext()

			if tt.wantCode != codes.OK {
				if len(publishContext) != 0 {
					t.Errorf("publish context on error: %v", publishContext)
				}
				return
			}

			if len(publishContext) != 1 {
				t.Fatalf("expected exactly one publish context key, got %v", publishContext)
			}
			if got := publishContext[devicePathContextKey]; got != tt.reported {
				t.Errorf(
					"expected publish context %s %q, got %q",
					devicePathContextKey,
					tt.reported,
					got,
				)
			}
		})
	}
}

func TestControllerPublishVolume_RepublishesTheSameDevicePath(t *testing.T) {
	t.Parallel()

	cloud := newMockEVSClient()
	svc, err := driver.NewControllerService(cloud, validTestConfig())
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}
	client := newControllerClient(t, svc)
	ctx := t.Context()

	vol, err := cloud.CreateVolume(ctx, evs.CreateVolumeOpts{
		Name:             "republished-volume",
		Size:             10,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: vol.ID,
		NodeId:   "9f0a1b2c-3d4e-5f60-7182-93a4b5c6d7e8",
		VolumeCapability: accessModeCapability(
			csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		),
	}

	first, err := client.ControllerPublishVolume(ctx, req)
	if err != nil {
		t.Fatalf("first publish failed: %v", err)
	}
	// Second publish is idempotent: same already-attached device path.
	second, err := client.ControllerPublishVolume(ctx, req)
	if err != nil {
		t.Fatalf("republish failed: %v", err)
	}

	if len(first.GetPublishContext()) != 1 {
		t.Fatalf("expected exactly one publish context key, got %v", first.GetPublishContext())
	}
	if !maps.Equal(first.GetPublishContext(), second.GetPublishContext()) {
		t.Errorf(
			"republish changed the publish context: %v then %v",
			first.GetPublishContext(),
			second.GetPublishContext(),
		)
	}
}

// byIDDirOf returns the emulated node's by-id directory.
func byIDDirOf(host *emulatedHost) string {
	return filepath.Dir(host.byIDLink)
}

// devDirOf returns the emulated node's device directory.
func devDirOf(host *emulatedHost) string {
	return filepath.Dir(host.devicePath)
}

// useCompleteSerialLink swaps the truncated by-id symlink for one that uses the full volume serial
// (some buses expose the full identifier in the serial field).
func useCompleteSerialLink(t *testing.T, host *emulatedHost) {
	t.Helper()

	if err := os.Remove(host.byIDLink); err != nil {
		t.Fatalf("failed to remove the truncated by-id link: %v", err)
	}
	link := filepath.Join(byIDDirOf(host), "scsi-"+emulatedVolumeID)
	if err := os.Symlink(host.devicePath, link); err != nil {
		t.Fatalf("failed to link the complete volume serial: %v", err)
	}
}

func TestNodeService_AcceptsACompleteSerialLinkOnEmulatedHost(t *testing.T) {
	t.Parallel()

	t.Run("staging a mounted volume", func(t *testing.T) {
		t.Parallel()

		host := newEmulatedHost(t)
		useCompleteSerialLink(t, host)
		client := newEmulatedNodeClient(t, host)

		stagingPath := filepath.Join(host.dir, "staging")
		if _, err := client.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
			VolumeId:          emulatedVolumeID,
			StagingTargetPath: stagingPath,
			PublishContext:    host.publishContext(),
			VolumeCapability:  mountCapability("ext4"),
		}); err != nil {
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
	})

	t.Run("staging with a truncated link beside the complete one", func(t *testing.T) {
		t.Parallel()

		// udev may create both truncated and full serial links for one volume.
		host := newEmulatedHost(t)
		link := filepath.Join(byIDDirOf(host), "scsi-"+emulatedVolumeID)
		if err := os.Symlink(host.devicePath, link); err != nil {
			t.Fatalf("failed to link the complete volume serial: %v", err)
		}
		client := newEmulatedNodeClient(t, host)

		stagingPath := filepath.Join(host.dir, "staging")
		if _, err := client.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
			VolumeId:          emulatedVolumeID,
			StagingTargetPath: stagingPath,
			PublishContext:    host.publishContext(),
			VolumeCapability:  mountCapability("ext4"),
		}); err != nil {
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
	})

	t.Run("publishing a raw block volume", func(t *testing.T) {
		t.Parallel()

		host := newEmulatedHost(t)
		useCompleteSerialLink(t, host)
		client := newEmulatedNodeClient(t, host)

		targetPath := filepath.Join(host.dir, "block-target", "device")
		if _, err := client.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
			VolumeId:          emulatedVolumeID,
			StagingTargetPath: filepath.Join(host.dir, "block-staging"),
			TargetPath:        targetPath,
			PublishContext:    host.publishContext(),
			VolumeCapability:  blockCapability(),
		}); err != nil {
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
	})
}

func TestNodePublishVolume_RequiresThePublishedDevicePathForRawBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		publishContext map[string]string
	}{
		{name: "no publish context", publishContext: nil},
		{name: "no device path", publishContext: map[string]string{"other": "/dev/vdb"}},
		{name: "blank device path", publishContext: map[string]string{devicePathContextKey: "   "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reachedHost := false
			fm := &fakeMounter{
				discoverDeviceFn: func(_ context.Context, _, _ string) (string, error) {
					reachedHost = true
					return "/dev/vdb", nil
				},
			}
			svc, err := driver.NewNodeService(fm, validTestConfig())
			if err != nil {
				t.Fatalf("NewNodeService failed: %v", err)
			}
			client := newNodeClient(t, svc)

			dir := t.TempDir()
			targetPath := filepath.Join(dir, "block-target", "device")
			_, err = client.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
				VolumeId:          emulatedVolumeID,
				StagingTargetPath: filepath.Join(dir, "block-staging"),
				TargetPath:        targetPath,
				PublishContext:    tt.publishContext,
				VolumeCapability:  blockCapability(),
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
			// Missing device path must fail before host device discovery.
			if reachedHost {
				t.Errorf("DiscoverDevice called without a published device path")
			}
			if _, statErr := os.Stat(targetPath); !os.IsNotExist(statErr) {
				t.Errorf("target path created on error: %s (err=%v)", targetPath, statErr)
			}
		})
	}
}

// unverifiedDevice is a node request case where device identity checks should fail.
type unverifiedDevice struct {
	name string
	// prepare mutates the emulated device tree and returns the publish context to send.
	prepare  func(t *testing.T, host *emulatedHost) map[string]string
	wantCode codes.Code
}

// unverifiedDevices lists publish-context / by-id mismatch cases that should not mount or format.
func unverifiedDevices() []unverifiedDevice {
	return []unverifiedDevice{
		{
			name: "the publish context carries no device path",
			prepare: func(_ *testing.T, _ *emulatedHost) map[string]string {
				return nil
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "the published device path is blank",
			prepare: func(_ *testing.T, _ *emulatedHost) map[string]string {
				return map[string]string{devicePathContextKey: "   "}
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "the published device path is outside the node device directory",
			prepare: func(t *testing.T, host *emulatedHost) map[string]string {
				outside := filepath.Join(host.dir, "not-a-device")
				if err := os.WriteFile(
					outside,
					[]byte("outside the device directory"),
					0o600,
				); err != nil {
					t.Fatalf("failed to create the outside file: %v", err)
				}
				return map[string]string{devicePathContextKey: outside}
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "the published device path is absent on this node",
			prepare: func(_ *testing.T, host *emulatedHost) map[string]string {
				return map[string]string{
					devicePathContextKey: filepath.Join(devDirOf(host), "vdz"),
				}
			},
			wantCode: codes.NotFound,
		},
		{
			name: "no by-id link identifies the volume",
			prepare: func(t *testing.T, host *emulatedHost) map[string]string {
				if err := os.Remove(host.byIDLink); err != nil {
					t.Fatalf("failed to remove the by-id link: %v", err)
				}
				return host.publishContext()
			},
			wantCode: codes.NotFound,
		},
		{
			name: "the volume by-id link names another kernel device",
			prepare: func(t *testing.T, host *emulatedHost) map[string]string {
				// Point the truncated serial link at a different device.
				unrelated := filepath.Join(devDirOf(host), "vdc")
				if err := os.WriteFile(unrelated, []byte("unrelated volume"), 0o600); err != nil {
					t.Fatalf("failed to create the unrelated device: %v", err)
				}
				if err := os.Remove(host.byIDLink); err != nil {
					t.Fatalf("failed to remove the by-id link: %v", err)
				}
				if err := os.Symlink(unrelated, host.byIDLink); err != nil {
					t.Fatalf("failed to relink the by-id link: %v", err)
				}
				return host.publishContext()
			},
			wantCode: codes.FailedPrecondition,
		},
	}
}

// assertHostUntouched fails if the host ran mkfs or mounted anything.
func assertHostUntouched(t *testing.T, host *emulatedHost) {
	t.Helper()

	if host.exec.countCalls("mkfs.") != 0 {
		t.Errorf("mkfs ran on error path, calls: %v", host.exec.calls)
	}
	if len(host.table.MountPoints) != 0 {
		t.Errorf("mount table not empty on error path: %+v", host.table.MountPoints)
	}
}

func TestNodeStageVolume_UnverifiedDeviceOnEmulatedHost(t *testing.T) {
	t.Parallel()

	for _, tt := range unverifiedDevices() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			host := newEmulatedHost(t)
			client := newEmulatedNodeClient(t, host)
			publishContext := tt.prepare(t, host)

			stagingPath := filepath.Join(host.dir, "staging")
			_, err := client.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
				VolumeId:          emulatedVolumeID,
				StagingTargetPath: stagingPath,
				PublishContext:    publishContext,
				VolumeCapability:  mountCapability("ext4"),
			})
			if status.Code(err) != tt.wantCode {
				t.Fatalf("expected status %v, got %v", tt.wantCode, err)
			}

			assertHostUntouched(t, host)
			if _, statErr := os.Stat(stagingPath); !os.IsNotExist(statErr) {
				t.Errorf("staging path created on error: %s (err=%v)", stagingPath, statErr)
			}
		})
	}
}

func TestNodePublishVolume_UnverifiedRawBlockDeviceOnEmulatedHost(t *testing.T) {
	t.Parallel()

	for _, tt := range unverifiedDevices() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			host := newEmulatedHost(t)
			client := newEmulatedNodeClient(t, host)
			publishContext := tt.prepare(t, host)

			targetPath := filepath.Join(host.dir, "block-target", "device")
			_, err := client.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
				VolumeId:          emulatedVolumeID,
				StagingTargetPath: filepath.Join(host.dir, "block-staging"),
				TargetPath:        targetPath,
				PublishContext:    publishContext,
				VolumeCapability:  blockCapability(),
			})
			if status.Code(err) != tt.wantCode {
				t.Fatalf("expected status %v, got %v", tt.wantCode, err)
			}

			assertHostUntouched(t, host)
			if _, statErr := os.Stat(targetPath); !os.IsNotExist(statErr) {
				t.Errorf("target path created on error: %s (err=%v)", targetPath, statErr)
			}
		})
	}
}
