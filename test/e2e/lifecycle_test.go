//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/reclaim"
)

const (
	// devicePathKey is the single publish-context key the controller
	// hands the node.
	devicePathKey = "devicePath"
	// parameterVolumeType is the one provisioning parameter the operator
	// must declare.
	parameterVolumeType = "type"

	bytesPerGiB = 1 << 30
)

func mountCapability(fsType string) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fsType},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func blockCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

// TestVolumeLifecycle drives each accepted access type end to end
// through the plugin surface, on the real device the cloud attached,
// and repeats every call that must be idempotent.
func TestVolumeLifecycle(t *testing.T) {
	ctx := runState.context()

	clause(t, catalogue.CheckExt4LifecycleCompletes, func(t *testing.T) {
		exerciseLifecycle(ctx, t, "ext4", mountCapability("ext4"))
	})

	clause(t, catalogue.CheckXFSLifecycleCompletes, func(t *testing.T) {
		exerciseLifecycle(ctx, t, "xfs", mountCapability("xfs"))
	})

	clause(t, catalogue.CheckBlockLifecycleCompletes, func(t *testing.T) {
		exerciseLifecycle(ctx, t, "block", blockCapability())
	})
}

// exerciseLifecycle provisions, attaches, stages, publishes, uses and releases one volume. It
// repeats each idempotent call to confirm a retry changes nothing.
func exerciseLifecycle(
	ctx context.Context,
	t *testing.T,
	label string,
	capability *csi.VolumeCapability,
) {
	t.Helper()

	h := runState
	controller := h.controller.controllerClient()
	node := h.node.nodeClient()

	volumeName := h.name("lc-" + label)
	createRequest := &csi.CreateVolumeRequest{
		Name:               volumeName,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: minimumVolumeGiB * bytesPerGiB},
		VolumeCapabilities: []*csi.VolumeCapability{capability},
		Parameters:         map[string]string{parameterVolumeType: h.env.volumeType},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{driver.TopologyZoneKey: h.identity.Zone}},
			},
		},
	}
	created := createTracked(ctx, t, controller, createRequest, "volume")
	volumeID := created.GetVolumeId()
	repeatedCreate := createTracked(ctx, t, controller, createRequest, "repeated volume")
	if !proto.Equal(created, repeatedCreate) {
		t.Fatal("a repeated create returned a different volume contract")
	}
	ids, err := h.findVolumeIDsByName(ctx, volumeName)
	if err != nil {
		t.Fatalf("list repeated create result: %v", err)
	}
	if len(ids) != 1 || ids[0] != volumeID {
		t.Fatalf("repeated create left %d exact-name volumes, want only %q", len(ids), volumeID)
	}
	h.evidence.Observe("lifecycle_"+label+"_volume", volumeID)

	h.reserveAttachment(t, volumeName, volumeID)

	publishRequest := &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           h.identity.ServerID,
		VolumeCapability: capability,
	}
	published, err := controller.ControllerPublishVolume(ctx, publishRequest)
	if err != nil {
		t.Fatalf("controller publish: %v", err)
	}
	publishContext := published.GetPublishContext()
	if publishContext[devicePathKey] == "" {
		t.Fatalf("controller published no device path")
	}
	h.evidence.Observe("lifecycle_"+label+"_device", publishContext[devicePathKey])

	repeated, err := controller.ControllerPublishVolume(ctx, publishRequest)
	if err != nil {
		t.Fatalf("controller publish repeated: %v", err)
	}
	if repeated.GetPublishContext()[devicePathKey] != publishContext[devicePathKey] {
		t.Fatalf("a repeated publish reported a different device path")
	}

	stagingPath := filepath.Join(h.runDir, label, "staging")
	targetPath := filepath.Join(h.runDir, label, "target")
	if err := os.MkdirAll(filepath.Dir(stagingPath), 0o750); err != nil {
		t.Fatalf("prepare paths: %v", err)
	}

	stageRequest := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		PublishContext:    publishContext,
		VolumeCapability:  capability,
	}
	mustSucceedTwice(ctx, t, "stage", node.NodeStageVolume, stageRequest)

	publishNodeRequest := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		PublishContext:    publishContext,
		VolumeCapability:  capability,
	}
	mustSucceedTwice(ctx, t, "node publish", node.NodePublishVolume, publishNodeRequest)

	if capability.GetMount() != nil {
		assertWritableFilesystem(t, targetPath)
	} else {
		assertRawBlock(t, targetPath)
	}

	unpublishNode := &csi.NodeUnpublishVolumeRequest{VolumeId: volumeID, TargetPath: targetPath}
	mustSucceedTwice(ctx, t, "node unpublish", node.NodeUnpublishVolume, unpublishNode)

	unstage := &csi.NodeUnstageVolumeRequest{VolumeId: volumeID, StagingTargetPath: stagingPath}
	mustSucceedTwice(ctx, t, "unstage", node.NodeUnstageVolume, unstage)

	controllerUnpublish := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   h.identity.ServerID,
	}
	mustSucceedTwice(ctx, t, "controller unpublish", controller.ControllerUnpublishVolume,
		controllerUnpublish)

	// A deletion the cloud has already applied is reported as success,
	// not as a missing volume.
	deleteRequest := &csi.DeleteVolumeRequest{VolumeId: volumeID}
	mustSucceedTwice(ctx, t, "delete", controller.DeleteVolume, deleteRequest)
}

// createTracked issues one CreateVolume under the reserve-before-create
// ledger discipline and returns the volume the controller reported.
func createTracked(
	ctx context.Context,
	t *testing.T,
	controller csi.ControllerClient,
	request *csi.CreateVolumeRequest,
	label string,
) *csi.Volume {
	t.Helper()

	h := runState
	if err := h.ledger.record(reclaim.Entry{
		Kind: reclaim.ResourceVolume,
		Name: request.GetName(),
	}); err != nil {
		t.Fatalf("reserve %s: %v", label, err)
	}
	created, err := controller.CreateVolume(ctx, request)
	if err != nil {
		t.Fatalf("create %s: %v", label, err)
	}
	volume := created.GetVolume()
	if err := h.ledger.recordVolumeID(request.GetName(), volume.GetVolumeId()); err != nil {
		t.Fatalf("record %s: %v", label, err)
	}
	return volume
}

// mustSucceedTwice issues the same request twice. A retry on this path must change nothing.
func mustSucceedTwice[Request, Response any](
	ctx context.Context,
	t *testing.T,
	label string,
	call func(context.Context, Request, ...grpc.CallOption) (Response, error),
	request Request,
) {
	t.Helper()

	if _, err := call(ctx, request); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if _, err := call(ctx, request); err != nil {
		t.Fatalf("%s repeated: %v", label, err)
	}
}

// assertWritableFilesystem confirms the published target holds a
// filesystem a workload can use.
func assertWritableFilesystem(t *testing.T, targetPath string) {
	t.Helper()

	probe := filepath.Join(targetPath, "probe")
	want := []byte("published")
	if err := os.WriteFile(probe, want, 0o600); err != nil {
		t.Fatalf("write to the published filesystem: %v", err)
	}
	//nolint:gosec // the path is built inside the target this run just published
	got, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read back from the published filesystem: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the published filesystem returned %q", got)
	}
}

// assertRawBlock confirms the published target is the device itself and
// is readable.
func assertRawBlock(t *testing.T, targetPath string) {
	t.Helper()

	block, err := isBlockDevice(targetPath)
	if err != nil {
		t.Fatalf("inspect the published target: %v", err)
	}
	if !block {
		t.Fatalf("the published raw target is not a block device")
	}

	//nolint:gosec // the path is the target this run just published
	file, err := os.Open(targetPath)
	if err != nil {
		t.Fatalf("open the published raw target: %v", err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Read(make([]byte, 512)); err != nil {
		t.Fatalf("read from the published raw target: %v", err)
	}
}
