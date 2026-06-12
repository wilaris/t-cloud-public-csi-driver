package driver_test

import (
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

// Seeded volume ID/name pair used to assert logs carry ID only, not the caller-chosen name.
const (
	adoptableVolumeID   = "8f2c1a4d-6b07-4e19-9a3f-2d5e7c018bb4"
	adoptableVolumeName = "pvc-caller-chosen-name"
)

// seedAdoptableVolume inserts a volume the controller will treat as adopt-able.
func seedAdoptableVolume(client *mockEVSClient) {
	client.volumes[adoptableVolumeID] = &evs.Volume{
		ID:               adoptableVolumeID,
		Name:             adoptableVolumeName,
		Status:           "available",
		Size:             10,
		AvailabilityZone: "eu-de-01",
		VolumeType:       "SSD",
		Tags:             map[string]string{evs.OwnershipTagKey: evs.OwnershipTagValue},
	}
}

// createVolumeRequest builds a 10GiB SSD CreateVolumeRequest in eu-de-01.
func createVolumeRequest(name string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:               name,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 10 * 1024 * 1024 * 1024},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability("")},
		Parameters:         map[string]string{"type": "SSD"},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{Segments: map[string]string{driver.TopologyZoneKey: "eu-de-01"}},
			},
		},
	}
}

func TestCreateVolumeSeparatesCreatingFromAdopting(t *testing.T) {
	t.Parallel()

	// Create and adopt both return the same response shape; only the log line differs.
	logger, buf := captureLogger()
	client := newMockEVSClient()
	svc, _ := driver.NewControllerService(client, validTestConfig(), logger)

	resp, err := svc.CreateVolume(t.Context(), createVolumeRequest("first-request"))
	if err != nil {
		t.Fatalf("unexpected error creating a volume: %v", err)
	}

	created := buf.String()
	if !strings.Contains(created, "Created a volume") {
		t.Errorf("missing create log: %s", created)
	}
	if strings.Contains(created, "Adopted an existing volume") {
		t.Errorf("create logged as adopt: %s", created)
	}
	if !strings.Contains(created, resp.GetVolume().GetVolumeId()) {
		t.Errorf("missing volume id in log: %s", created)
	}

	buf.Reset()

	// Repeat create should adopt the volume created above.
	if _, err := svc.CreateVolume(t.Context(), createVolumeRequest("first-request")); err != nil {
		t.Fatalf("unexpected error on the repeated request: %v", err)
	}

	adopted := buf.String()
	if !strings.Contains(adopted, "Adopted an existing volume") {
		t.Errorf("missing adopt log: %s", adopted)
	}
	if strings.Contains(adopted, "Created a volume") {
		t.Errorf("adopt logged as create: %s", adopted)
	}
}

func TestCreateVolumeRecordsNoCallerChosenName(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogger()
	client := newMockEVSClient()
	seedAdoptableVolume(client)
	svc, _ := driver.NewControllerService(client, validTestConfig(), logger)

	_, err := svc.CreateVolume(t.Context(), createVolumeRequest(adoptableVolumeName))
	if err != nil {
		t.Fatalf("unexpected error adopting the seeded volume: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, adoptableVolumeID) {
		t.Errorf("missing volume id in log: %s", out)
	}
	if strings.Contains(out, adoptableVolumeName) {
		t.Errorf("volume name leaked in log: %s", out)
	}
}

func TestDeleteVolumeSeparatesDestroyingFromFindingNothing(t *testing.T) {
	t.Parallel()

	// Delete is idempotent; second call should log "already absent", not "Deleted".
	logger, buf := captureLogger()
	client := newMockEVSClient()
	seedAdoptableVolume(client)
	svc, _ := driver.NewControllerService(client, validTestConfig(), logger)

	req := &csi.DeleteVolumeRequest{VolumeId: adoptableVolumeID}

	if _, err := svc.DeleteVolume(t.Context(), req); err != nil {
		t.Fatalf("unexpected error deleting a volume: %v", err)
	}

	deleted := buf.String()
	if !strings.Contains(deleted, "Deleted a volume") {
		t.Errorf("missing delete log: %s", deleted)
	}
	if !strings.Contains(deleted, adoptableVolumeID) {
		t.Errorf("missing volume id in log: %s", deleted)
	}

	buf.Reset()

	if _, err := svc.DeleteVolume(t.Context(), req); err != nil {
		t.Fatalf("unexpected error on the repeated delete: %v", err)
	}

	absent := buf.String()
	if !strings.Contains(absent, "Volume was already absent") {
		t.Errorf("missing already-absent log: %s", absent)
	}
	if strings.Contains(absent, "Deleted a volume") {
		t.Errorf("absent volume logged as delete: %s", absent)
	}
}

func TestControllerRecordsNothingForAPublishedVolume(t *testing.T) {
	t.Parallel()

	// Publish response already includes the device path; no extra success log expected.
	logger, buf := captureLogger()
	client := newMockEVSClient()
	seedAdoptableVolume(client)
	svc, _ := driver.NewControllerService(client, validTestConfig(), logger)

	_, err := svc.ControllerPublishVolume(t.Context(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         adoptableVolumeID,
		NodeId:           validTestConfig().NodeID,
		VolumeCapability: mountCapability(""),
	})
	if err != nil {
		t.Fatalf("unexpected error publishing a volume: %v", err)
	}

	if out := buf.String(); out != "" {
		t.Errorf("unexpected log on publish: %s", out)
	}
}
