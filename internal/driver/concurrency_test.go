package driver_test

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	mountutils "k8s.io/mount-utils"
	k8sexec "k8s.io/utils/exec"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/mount"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/mount/mounttest"
)

// concurrentEVSClient is a thread-safe in-memory test double for EVSClient operations.
type concurrentEVSClient struct {
	mu          sync.RWMutex
	volumes     map[string]*evs.Volume
	attachments map[string]string // volumeID -> serverID
	createCalls int
}

func newConcurrentEVSClient() *concurrentEVSClient {
	return &concurrentEVSClient{
		volumes:     make(map[string]*evs.Volume),
		attachments: make(map[string]string),
	}
}

func (c *concurrentEVSClient) CreateVolume(
	ctx context.Context,
	opts evs.CreateVolumeOpts,
) (*evs.Volume, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.createCalls++
	vol := &evs.Volume{
		ID:               fmt.Sprintf("vol-%s", opts.Name),
		Name:             opts.Name,
		Status:           "available",
		Size:             opts.Size,
		AvailabilityZone: opts.AvailabilityZone,
		VolumeType:       opts.VolumeType,
		Tags:             map[string]string{evs.OwnershipTagKey: evs.OwnershipTagValue},
	}
	c.volumes[vol.ID] = vol
	return vol, nil
}

func (c *concurrentEVSClient) GetVolume(
	ctx context.Context,
	id string,
) (*evs.Volume, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	vol, ok := c.volumes[id]
	if !ok {
		return nil, fmt.Errorf("volume %s: %w", id, evs.ErrNotFound)
	}
	return vol, nil
}

func (c *concurrentEVSClient) DiscoverVolume(
	ctx context.Context,
	opts evs.DiscoverVolumeOpts,
) (*evs.Volume, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	nameExists := false
	for _, vol := range c.volumes {
		if vol.Name != opts.Name {
			continue
		}
		nameExists = true
		if vol.Tags[evs.OwnershipTagKey] != evs.OwnershipTagValue {
			continue
		}
		if vol.Status != "available" && vol.Status != "in-use" {
			continue
		}
		if vol.AvailabilityZone != opts.AvailabilityZone || vol.VolumeType != opts.VolumeType {
			continue
		}
		if vol.Size < opts.MinSizeGiB {
			continue
		}
		if opts.MaxSizeGiB > 0 && vol.Size > opts.MaxSizeGiB {
			continue
		}
		return vol, nil
	}
	if nameExists {
		return nil, fmt.Errorf("volume %s: %w", opts.Name, evs.ErrConflict)
	}
	return nil, fmt.Errorf("volume %s: %w", opts.Name, evs.ErrNotFound)
}

func (c *concurrentEVSClient) DeleteVolume(
	ctx context.Context,
	id string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	vol, ok := c.volumes[id]
	if !ok {
		return fmt.Errorf("volume %s: %w", id, evs.ErrNotFound)
	}
	if vol.Tags[evs.OwnershipTagKey] != evs.OwnershipTagValue {
		return fmt.Errorf("volume %s: %w", id, evs.ErrNotOwned)
	}
	if vol.Status == "in-use" {
		return fmt.Errorf("volume %s: %w", id, evs.ErrConflict)
	}
	delete(c.volumes, id)
	return nil
}

func (c *concurrentEVSClient) AttachVolume(
	ctx context.Context,
	volumeID, serverID string,
) (*evs.Attachment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	vol, ok := c.volumes[volumeID]
	if !ok {
		return nil, fmt.Errorf("volume %s: %w", volumeID, evs.ErrNotFound)
	}
	if existingServer, ok := c.attachments[volumeID]; ok {
		if existingServer == serverID {
			return &evs.Attachment{
				ServerID:   serverID,
				DeviceName: mockAttachedDeviceName,
			}, nil
		}
		return nil, fmt.Errorf("volume %s: %w", volumeID, evs.ErrConflict)
	}
	vol.Status = "in-use"
	c.attachments[volumeID] = serverID
	return &evs.Attachment{
		ServerID:   serverID,
		DeviceName: mockAttachedDeviceName,
	}, nil
}

func (c *concurrentEVSClient) DetachVolume(
	ctx context.Context,
	volumeID, serverID string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	attachedServer, ok := c.attachments[volumeID]
	if !ok {
		return nil
	}
	if attachedServer != serverID {
		return fmt.Errorf("volume %s: %w", volumeID, evs.ErrConflict)
	}
	delete(c.attachments, volumeID)
	if vol, ok := c.volumes[volumeID]; ok {
		vol.Status = "available"
	}
	return nil
}

// synchronizedMounter synchronizes all mount table operations.
type synchronizedMounter struct {
	mu sync.Mutex
	mountutils.Interface
	base *mountutils.FakeMounter
}

func newSynchronizedMounter() *synchronizedMounter {
	base := mountutils.NewFakeMounter(nil)
	return &synchronizedMounter{
		Interface: base,
		base:      base,
	}
}

func (s *synchronizedMounter) Mount(
	source, target, fsType string,
	options []string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base.Mount(source, target, fsType, options)
}

func (s *synchronizedMounter) MountSensitive(
	source, target, fsType string,
	options, sensitiveOptions []string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base.MountSensitive(source, target, fsType, options, sensitiveOptions)
}

func (s *synchronizedMounter) MountSensitiveWithoutSystemd(
	source, target, fsType string,
	options, sensitiveOptions []string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base.MountSensitiveWithoutSystemd(source, target, fsType, options, sensitiveOptions)
}

func (s *synchronizedMounter) MountSensitiveWithoutSystemdWithMountFlags(
	source, target, fsType string,
	options, sensitiveOptions, mountFlags []string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base.MountSensitiveWithoutSystemdWithMountFlags(
		source,
		target,
		fsType,
		options,
		sensitiveOptions,
		mountFlags,
	)
}

func (s *synchronizedMounter) Unmount(target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base.Unmount(target)
}

func (s *synchronizedMounter) List() ([]mountutils.MountPoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base.List()
}

func (s *synchronizedMounter) IsMountPoint(file string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base.IsMountPoint(file)
}

func (s *synchronizedMounter) IsLikelyNotMountPoint(file string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base.IsLikelyNotMountPoint(file)
}

// concurrentHostCmd answers one block-device command with mutex synchronization.
type concurrentHostCmd struct {
	mu      *sync.Mutex
	args    []string
	handler func(args ...string) ([]byte, error)
}

func (c *concurrentHostCmd) CombinedOutput() ([]byte, error) {
	if c.mu != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
	}
	if c.handler != nil {
		return c.handler(c.args...)
	}
	return nil, nil
}

func (c *concurrentHostCmd) Output() ([]byte, error) { return c.CombinedOutput() }

func (c *concurrentHostCmd) Run() error                         { _, err := c.CombinedOutput(); return err }
func (c *concurrentHostCmd) Start() error                       { return nil }
func (c *concurrentHostCmd) Wait() error                        { return nil }
func (c *concurrentHostCmd) Stop()                              {}
func (c *concurrentHostCmd) SetDir(_ string)                    {}
func (c *concurrentHostCmd) SetStdin(_ io.Reader)               {}
func (c *concurrentHostCmd) SetStdout(_ io.Writer)              {}
func (c *concurrentHostCmd) SetStderr(_ io.Writer)              {}
func (c *concurrentHostCmd) SetEnv(_ []string)                  {}
func (c *concurrentHostCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (c *concurrentHostCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }

// concurrentHostExec is a thread-safe command execution double for mount tests.
type concurrentHostExec struct {
	mu       *sync.Mutex
	handlers map[string]func(args ...string) ([]byte, error)
	calls    []string
}

func (e *concurrentHostExec) Command(cmd string, args ...string) k8sexec.Cmd {
	e.mu.Lock()
	e.calls = append(e.calls, strings.TrimSpace(cmd+" "+strings.Join(args, " ")))
	handler := e.handlers[cmd]
	e.mu.Unlock()
	return &concurrentHostCmd{mu: e.mu, args: args, handler: handler}
}

func (e *concurrentHostExec) CommandContext(
	_ context.Context,
	cmd string,
	args ...string,
) k8sexec.Cmd {
	return e.Command(cmd, args...)
}

func (e *concurrentHostExec) LookPath(file string) (string, error) {
	return file, nil
}

// concurrentEmulatedHost sets up a thread-safe environment for concurrent Node service testing.
type concurrentEmulatedHost struct {
	mounter *mount.LinuxMounter
	table   *synchronizedMounter
	exec    *concurrentHostExec
	devDir  string
	byIDDir string
	dir     string
}

func newConcurrentEmulatedHost(t *testing.T) *concurrentEmulatedHost {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to resolve temporary directory: %v", err)
	}

	devDir := filepath.Join(dir, "dev")
	byIDDir := filepath.Join(devDir, "disk", "by-id")
	if err := os.MkdirAll(byIDDir, 0o750); err != nil {
		t.Fatalf("failed to create by-id directory: %v", err)
	}

	table := newSynchronizedMounter()
	hostCommands := &concurrentHostExec{
		mu: &table.mu,
		handlers: map[string]func(args ...string) ([]byte, error){
			"blkid":     func(_ ...string) ([]byte, error) { return []byte(""), nil },
			"lsblk":     func(_ ...string) ([]byte, error) { return []byte(""), nil },
			"mkfs.ext4": func(_ ...string) ([]byte, error) { return []byte("done"), nil },
			"mkfs.xfs":  func(_ ...string) ([]byte, error) { return []byte("done"), nil },
		},
	}
	maps.Copy(hostCommands.handlers, mounttest.Commands(table.base))

	linuxMounter := mount.NewLinuxMounter(
		mount.WithMountUtilsInterface(table),
		mount.WithExecInterface(hostCommands),
		mount.WithDevDir(devDir),
		mount.WithDiskByIDDir(byIDDir),
		mount.WithStatfsFunc(func(_ string, buf *syscall.Statfs_t) error {
			buf.Flags = 0
			return nil
		}),
	)

	return &concurrentEmulatedHost{
		mounter: linuxMounter,
		table:   table,
		exec:    hostCommands,
		devDir:  devDir,
		byIDDir: byIDDir,
		dir:     dir,
	}
}

func (h *concurrentEmulatedHost) registerDevice(t *testing.T, volumeID, devName string) string {
	t.Helper()

	devPath := filepath.Join(h.devDir, devName)
	if err := os.WriteFile(devPath, []byte("volume-"+volumeID), 0o600); err != nil {
		t.Fatalf("failed to create device node: %v", err)
	}

	serial := volumeID
	if len(serial) > 20 {
		serial = serial[:20]
	}
	byIDLink := filepath.Join(h.byIDDir, "virtio-"+serial)
	if err := os.Symlink(devPath, byIDLink); err != nil && !os.IsExist(err) {
		t.Fatalf("failed to create symlink: %v", err)
	}

	return devPath
}

func TestIdentityServiceConcurrency(t *testing.T) {
	cfg := validTestConfig()
	svc, err := driver.NewIdentityService(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewIdentityService() = %v", err)
	}

	const workers = 25
	const iterationsPerWorker = 40

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for j := 0; j < iterationsPerWorker; j++ {
				ctx := context.Background()

				infoResp, infoErr := svc.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
				if infoErr != nil {
					t.Errorf("GetPluginInfo() unexpected error: %v", infoErr)
					return
				}
				if infoResp.GetName() != cfg.DriverName {
					t.Errorf(
						"GetPluginInfo().Name = %q, want %q",
						infoResp.GetName(),
						cfg.DriverName,
					)
				}
				if infoResp.GetVendorVersion() != cfg.Version {
					t.Errorf(
						"GetPluginInfo().VendorVersion = %q, want %q",
						infoResp.GetVendorVersion(),
						cfg.Version,
					)
				}

				capsResp, capsErr := svc.GetPluginCapabilities(
					ctx,
					&csi.GetPluginCapabilitiesRequest{},
				)
				if capsErr != nil {
					t.Errorf("GetPluginCapabilities() unexpected error: %v", capsErr)
					return
				}
				if len(capsResp.GetCapabilities()) == 0 {
					t.Errorf("GetPluginCapabilities() returned empty capabilities")
				}

				probeResp, probeErr := svc.Probe(ctx, &csi.ProbeRequest{})
				if probeErr != nil {
					t.Errorf("Probe() unexpected error: %v", probeErr)
					return
				}
				if !probeResp.GetReady().GetValue() {
					t.Errorf("Probe().Ready = false, want true")
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}

func TestControllerServiceConcurrency_RacingCreateVolume(t *testing.T) {
	evsClient := newConcurrentEVSClient()
	cfg := validTestConfig()
	svc, err := driver.NewControllerService(evsClient, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService() = %v", err)
	}

	const volumeName = "concurrent-racing-vol"
	const workers = 30

	var wg sync.WaitGroup
	start := make(chan struct{})
	responses := make([]*csi.CreateVolumeResponse, workers)
	errorsList := make([]error, workers)

	for i := 0; i < workers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			req := &csi.CreateVolumeRequest{
				Name: volumeName,
				VolumeCapabilities: []*csi.VolumeCapability{
					mountCapability("ext4"),
				},
				Parameters: map[string]string{
					"type":              "SAS",
					"availability_zone": "eu-de-01",
				},
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: 10 * 1024 * 1024 * 1024,
				},
			}

			resp, reqErr := svc.CreateVolume(context.Background(), req)
			responses[workerID] = resp
			errorsList[workerID] = reqErr
		}()
	}

	close(start)
	wg.Wait()

	wantVolumeID := "vol-" + volumeName
	for i := 0; i < workers; i++ {
		if errorsList[i] != nil {
			t.Errorf("worker %d CreateVolume() error: %v", i, errorsList[i])
			continue
		}
		if responses[i] == nil || responses[i].GetVolume() == nil {
			t.Errorf("worker %d returned nil volume response", i)
			continue
		}
		if got := responses[i].GetVolume().GetVolumeId(); got != wantVolumeID {
			t.Errorf("worker %d VolumeId = %q, want %q", i, got, wantVolumeID)
		}
		if got := responses[i].GetVolume().GetCapacityBytes(); got != 10*1024*1024*1024 {
			t.Errorf("worker %d CapacityBytes = %d, want %d", i, got, 10*1024*1024*1024)
		}
	}
}

func TestControllerServiceConcurrency_IdempotentAdoption(t *testing.T) {
	evsClient := newConcurrentEVSClient()
	cfg := validTestConfig()
	svc, err := driver.NewControllerService(evsClient, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService() = %v", err)
	}

	const volumeName = "precreated-vol"
	_, err = svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: volumeName,
		VolumeCapabilities: []*csi.VolumeCapability{
			mountCapability("ext4"),
		},
		Parameters: map[string]string{
			"type":              "SAS",
			"availability_zone": "eu-de-01",
		},
	})
	if err != nil {
		t.Fatalf("initial CreateVolume failed: %v", err)
	}

	const workers = 25
	var wg sync.WaitGroup
	start := make(chan struct{})
	errorsList := make([]error, workers)
	responses := make([]*csi.CreateVolumeResponse, workers)

	for i := 0; i < workers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			resp, reqErr := svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name: volumeName,
				VolumeCapabilities: []*csi.VolumeCapability{
					mountCapability("ext4"),
				},
				Parameters: map[string]string{
					"type":              "SAS",
					"availability_zone": "eu-de-01",
				},
			})
			responses[workerID] = resp
			errorsList[workerID] = reqErr
		}()
	}

	close(start)
	wg.Wait()

	wantVolumeID := "vol-" + volumeName
	for i := 0; i < workers; i++ {
		if errorsList[i] != nil {
			t.Errorf("worker %d CreateVolume() error: %v", i, errorsList[i])
			continue
		}
		if responses[i] == nil || responses[i].GetVolume() == nil {
			t.Errorf("worker %d returned nil volume response", i)
			continue
		}
		if got := responses[i].GetVolume().GetVolumeId(); got != wantVolumeID {
			t.Errorf("worker %d VolumeId = %q, want %q", i, got, wantVolumeID)
		}
	}

	evsClient.mu.RLock()
	defer evsClient.mu.RUnlock()
	if evsClient.createCalls != 1 {
		t.Errorf("createCalls = %d, want exactly 1 (others adopted)", evsClient.createCalls)
	}
}

func TestControllerServiceConcurrency_MixedLifecycle(t *testing.T) {
	evsClient := newConcurrentEVSClient()
	cfg := validTestConfig()
	svc, err := driver.NewControllerService(evsClient, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService() = %v", err)
	}

	const workers = 20
	const iterations = 10

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for iter := 0; iter < iterations; iter++ {
				ctx := context.Background()
				volName := fmt.Sprintf("mixed-vol-w%d-i%d", workerID, iter)
				nodeID := fmt.Sprintf("node-w%d", workerID)

				createResp, createErr := svc.CreateVolume(ctx, &csi.CreateVolumeRequest{
					Name: volName,
					VolumeCapabilities: []*csi.VolumeCapability{
						mountCapability("ext4"),
					},
					Parameters: map[string]string{
						"type":              "SAS",
						"availability_zone": "eu-de-01",
					},
				})
				if createErr != nil {
					t.Errorf("worker %d CreateVolume(%s) error: %v", workerID, volName, createErr)
					return
				}
				volID := createResp.GetVolume().GetVolumeId()

				valResp, valErr := svc.ValidateVolumeCapabilities(
					ctx,
					&csi.ValidateVolumeCapabilitiesRequest{
						VolumeId: volID,
						VolumeCapabilities: []*csi.VolumeCapability{
							mountCapability("ext4"),
						},
						Parameters: map[string]string{
							"type":              "SAS",
							"availability_zone": "eu-de-01",
						},
					},
				)
				if valErr != nil {
					t.Errorf("worker %d ValidateVolumeCapabilities error: %v", workerID, valErr)
					return
				}
				if valResp.GetConfirmed() == nil {
					t.Errorf("worker %d ValidateVolumeCapabilities not confirmed", workerID)
				}

				pubResp, pubErr := svc.ControllerPublishVolume(
					ctx,
					&csi.ControllerPublishVolumeRequest{
						VolumeId: volID,
						NodeId:   nodeID,
						VolumeCapability: mountCapability(
							"ext4",
						),
					},
				)
				if pubErr != nil {
					t.Errorf("worker %d ControllerPublishVolume error: %v", workerID, pubErr)
					return
				}
				if pubResp.GetPublishContext()["devicePath"] != mockAttachedDeviceName {
					t.Errorf(
						"worker %d devicePath = %q, want %q",
						workerID,
						pubResp.GetPublishContext()["devicePath"],
						mockAttachedDeviceName,
					)
				}

				_, unpubErr := svc.ControllerUnpublishVolume(
					ctx,
					&csi.ControllerUnpublishVolumeRequest{
						VolumeId: volID,
						NodeId:   nodeID,
					},
				)
				if unpubErr != nil {
					t.Errorf("worker %d ControllerUnpublishVolume error: %v", workerID, unpubErr)
					return
				}

				_, delErr := svc.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
					VolumeId: volID,
				})
				if delErr != nil {
					t.Errorf("worker %d DeleteVolume error: %v", workerID, delErr)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}

func TestControllerServiceConcurrency_CancellationIsolation(t *testing.T) {
	evsClient := newConcurrentEVSClient()
	cfg := validTestConfig()
	svc, err := driver.NewControllerService(evsClient, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService() = %v", err)
	}

	const activeWorkers = 15
	const cancelledWorkers = 15

	var wg sync.WaitGroup
	start := make(chan struct{})

	// Active workers should succeed cleanly without interference.
	for i := 0; i < activeWorkers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			volName := fmt.Sprintf("active-vol-%d", workerID)
			resp, createErr := svc.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name: volName,
				VolumeCapabilities: []*csi.VolumeCapability{
					mountCapability("ext4"),
				},
				Parameters: map[string]string{
					"type":              "SAS",
					"availability_zone": "eu-de-01",
				},
			})
			if createErr != nil {
				t.Errorf("active worker %d CreateVolume error: %v", workerID, createErr)
				return
			}
			if resp.GetVolume().GetVolumeId() != "vol-"+volName {
				t.Errorf(
					"active worker %d unexpected volume ID: %s",
					workerID,
					resp.GetVolume().GetVolumeId(),
				)
			}
		}()
	}

	// Cancelled workers should promptly return canceled/deadline status without panic.
	for i := 0; i < cancelledWorkers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // immediate cancellation

			volName := fmt.Sprintf("canceled-vol-%d", workerID)
			_, createErr := svc.CreateVolume(ctx, &csi.CreateVolumeRequest{
				Name: volName,
				VolumeCapabilities: []*csi.VolumeCapability{
					mountCapability("ext4"),
				},
				Parameters: map[string]string{
					"type":              "SAS",
					"availability_zone": "eu-de-01",
				},
			})
			if createErr == nil {
				t.Errorf("canceled worker %d expected error, got nil", workerID)
				return
			}
			st, ok := status.FromError(createErr)
			if !ok || st.Code() != codes.Canceled {
				t.Errorf("canceled worker %d status = %v, want Canceled", workerID, createErr)
			}
		}()
	}

	close(start)
	wg.Wait()
}

func TestNodeServiceConcurrency_StagingAndPublishing(t *testing.T) {
	host := newConcurrentEmulatedHost(t)
	cfg := validTestConfig()
	svc, err := driver.NewNodeService(host.mounter, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewNodeService() = %v", err)
	}

	const workers = 15
	const iterations = 5

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		workerID := i
		volID := fmt.Sprintf("node-vol-%02d", workerID)
		devName := fmt.Sprintf("vd%c", 'b'+workerID)
		devPath := host.registerDevice(t, volID, devName)

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for iter := 0; iter < iterations; iter++ {
				ctx := context.Background()
				stagingPath := filepath.Join(host.dir, fmt.Sprintf("stage-w%d-i%d", workerID, iter))
				targetPath := filepath.Join(host.dir, fmt.Sprintf("target-w%d-i%d", workerID, iter))
				pubCtx := map[string]string{"devicePath": devPath}

				// Stage
				_, stageErr := svc.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
					VolumeId:          volID,
					StagingTargetPath: stagingPath,
					PublishContext:    pubCtx,
					VolumeCapability:  mountCapability("ext4"),
				})
				if stageErr != nil {
					t.Errorf("worker %d NodeStageVolume error: %v", workerID, stageErr)
					return
				}

				// Publish
				_, pubErr := svc.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
					VolumeId:          volID,
					StagingTargetPath: stagingPath,
					TargetPath:        targetPath,
					PublishContext:    pubCtx,
					VolumeCapability:  mountCapability("ext4"),
				})
				if pubErr != nil {
					t.Errorf("worker %d NodePublishVolume error: %v", workerID, pubErr)
					return
				}

				// Unpublish
				_, unpubErr := svc.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
					VolumeId:   volID,
					TargetPath: targetPath,
				})
				if unpubErr != nil {
					t.Errorf("worker %d NodeUnpublishVolume error: %v", workerID, unpubErr)
					return
				}

				// Unstage
				_, unstageErr := svc.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
					VolumeId:          volID,
					StagingTargetPath: stagingPath,
				})
				if unstageErr != nil {
					t.Errorf("worker %d NodeUnstageVolume error: %v", workerID, unstageErr)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}

func TestNodeServiceConcurrency_CancellationIsolation(t *testing.T) {
	host := newConcurrentEmulatedHost(t)
	cfg := validTestConfig()
	svc, err := driver.NewNodeService(host.mounter, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewNodeService() = %v", err)
	}

	const workers = 15
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		workerID := i
		volID := fmt.Sprintf("cancel-vol-%02d", workerID)
		devName := fmt.Sprintf("vd%c", 'b'+workerID)
		devPath := host.registerDevice(t, volID, devName)

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // Pre-canceled

			stagingPath := filepath.Join(host.dir, fmt.Sprintf("cancel-stage-%d", workerID))
			_, stageErr := svc.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
				VolumeId:          volID,
				StagingTargetPath: stagingPath,
				PublishContext:    map[string]string{"devicePath": devPath},
				VolumeCapability:  mountCapability("ext4"),
			})
			if stageErr == nil {
				t.Errorf("worker %d expected canceled error, got nil", workerID)
				return
			}
			st, ok := status.FromError(stageErr)
			if !ok || st.Code() != codes.Canceled {
				t.Errorf("worker %d status = %v, want Canceled", workerID, stageErr)
			}
		}()
	}

	close(start)
	wg.Wait()
}

func TestDriverConcurrency_OverGRPCTransport(t *testing.T) {
	evsClient := newConcurrentEVSClient()
	cfg := validTestConfig()

	identitySvc, err := driver.NewIdentityService(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewIdentityService() = %v", err)
	}
	controllerSvc, err := driver.NewControllerService(evsClient, cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewControllerService() = %v", err)
	}

	ln := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, identitySvc)
	csi.RegisterControllerServer(server, controllerSvc)

	go func() {
		if serveErr := server.Serve(ln); serveErr != nil && serveErr != grpc.ErrServerStopped {
			t.Errorf("gRPC serve error: %v", serveErr)
		}
	}()

	dialer := func(context.Context, string) (net.Conn, error) {
		return ln.Dial()
	}

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() = %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		server.GracefulStop()
		_ = ln.Close()
	})

	identityClient := csi.NewIdentityClient(conn)
	controllerClient := csi.NewControllerClient(conn)

	const workers = 25
	const iterations = 15

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for iter := 0; iter < iterations; iter++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				// Identity probe
				probeResp, probeErr := identityClient.Probe(ctx, &csi.ProbeRequest{})
				if probeErr != nil {
					t.Errorf("worker %d Probe() error: %v", workerID, probeErr)
					return
				}
				if !probeResp.GetReady().GetValue() {
					t.Errorf("worker %d Probe() not ready", workerID)
				}

				// Controller capabilities
				_, capErr := controllerClient.ControllerGetCapabilities(
					ctx,
					&csi.ControllerGetCapabilitiesRequest{},
				)
				if capErr != nil {
					t.Errorf("worker %d ControllerGetCapabilities error: %v", workerID, capErr)
					return
				}

				// Create Volume
				volName := fmt.Sprintf("grpc-vol-w%d-i%d", workerID, iter)
				createResp, createErr := controllerClient.CreateVolume(
					ctx,
					&csi.CreateVolumeRequest{
						Name: volName,
						VolumeCapabilities: []*csi.VolumeCapability{
							mountCapability("ext4"),
						},
						Parameters: map[string]string{
							"type":              "SAS",
							"availability_zone": "eu-de-01",
						},
					},
				)
				if createErr != nil {
					t.Errorf("worker %d CreateVolume(%s) error: %v", workerID, volName, createErr)
					return
				}

				volID := createResp.GetVolume().GetVolumeId()

				// Delete Volume
				_, delErr := controllerClient.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
					VolumeId: volID,
				})
				if delErr != nil {
					t.Errorf("worker %d DeleteVolume(%s) error: %v", workerID, volID, delErr)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}
