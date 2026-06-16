//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/opentelekomcloud/gophertelekomcloud/openstack/evs/v2/cloudvolumes"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/evs/v2/tags"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

// TestVolumeContract exercises volume operations against the real service, including adoption
// refusals and deletion branches that the plugin surface cannot request.
func TestVolumeContract(t *testing.T) {
	h := runState
	ctx := h.context()

	clause(t, catalogue.CheckMarkerOnDetailSurface, func(t *testing.T) {
		volume, err := h.createMarkedVolume(ctx, "marker", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		h.evidence.Observe("marker_volume_id", volume.ID)

		detail, err := h.evs.GetVolume(ctx, volume.ID)
		if err != nil {
			t.Fatalf("read detail: %v", err)
		}
		if detail.Tags[evs.OwnershipTagKey] != evs.OwnershipTagValue {
			t.Fatalf(
				"detail surface reports tags %v, without the ownership marker",
				detail.Tags,
			)
		}
		if detail.Status != statusAvailable {
			t.Fatalf("a freshly created volume reports status %q", detail.Status)
		}
	})

	clause(t, catalogue.CheckMarkerOnListSurface, func(t *testing.T) {
		volume, err := h.createMarkedVolume(ctx, "marker-list", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		listed, err := h.evs.ListVolumes(ctx, evs.ListVolumeOpts{Name: volume.Name})
		if err != nil {
			t.Fatalf("list: %v", err)
		}

		for i := range listed {
			if listed[i].ID != volume.ID {
				continue
			}
			if listed[i].Tags[evs.OwnershipTagKey] != evs.OwnershipTagValue {
				t.Fatalf(
					"list surface reports tags %v, without the ownership marker",
					listed[i].Tags,
				)
			}
			return
		}
		t.Fatalf("the name query did not return the volume just created")
	})

	clause(t, catalogue.CheckOffsetPagingAdvances, func(t *testing.T) {
		// The clause pages over a population it creates itself,
		// name-filtered, so a prior clause failure or another
		// actor in the project cannot fake a paging defect.
		base, err := h.createMarkedVolume(ctx, "paging", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create first volume: %v", err)
		}
		other, err := h.createMarkedVolumeNamed(ctx, base.Name, minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create second volume: %v", err)
		}

		first, err := h.evs.ListVolumes(
			ctx,
			evs.ListVolumeOpts{Name: base.Name, Limit: 1, Offset: 0},
		)
		if err != nil {
			t.Fatalf("read first page: %v", err)
		}
		second, err := h.evs.ListVolumes(
			ctx,
			evs.ListVolumeOpts{Name: base.Name, Limit: 1, Offset: 1},
		)
		if err != nil {
			t.Fatalf("read second page: %v", err)
		}
		if len(first) != 1 || len(second) != 1 {
			t.Fatalf(
				"a bounded page request returned %d and %d volumes",
				len(first),
				len(second),
			)
		}
		if first[0].ID == second[0].ID {
			t.Fatalf("offset did not advance: both pages report %s", first[0].ID)
		}
		created := map[string]bool{base.ID: true, other.ID: true}
		if !created[first[0].ID] || !created[second[0].ID] {
			t.Fatalf(
				"the name-filtered pages report %s and %s, not the two volumes this check created",
				first[0].ID,
				second[0].ID,
			)
		}
		h.evidence.Observe("paging_first_page_volume", first[0].ID)
		h.evidence.Observe("paging_second_page_volume", second[0].ID)
	})

	clause(t, catalogue.CheckUnmarkedNotAdopted, func(t *testing.T) {
		id, err := h.createUnmarkedVolume(ctx, "unmarked", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		before, err := h.getVolumeDirect(ctx, id)
		if err != nil {
			t.Fatalf("read before: %v", err)
		}

		discoverExpectConflict(ctx, t, before.Name, minimumVolumeGiB)
		assertUnchanged(ctx, t, id, before.Status)
	})

	clause(t, catalogue.CheckIncompatibleRefused, func(t *testing.T) {
		volume, err := h.createMarkedVolume(ctx, "incompatible", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		// The candidate carries the marker and is healthy. Only the requested size disagrees.
		discoverExpectConflict(ctx, t, volume.Name, minimumVolumeGiB*2)
		assertUnchanged(ctx, t, volume.ID, statusAvailable)
	})

	clause(t, catalogue.CheckDuplicateIsDeterministic, func(t *testing.T) {
		first, err := h.createMarkedVolume(ctx, "duplicate", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create first: %v", err)
		}
		second, err := h.createMarkedVolumeNamed(ctx, first.Name, minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create second: %v", err)
		}
		if first.ID == second.ID {
			t.Fatalf("the service collapsed two creates onto one volume")
		}

		opts := h.discoverOpts(first.Name, minimumVolumeGiB)
		adopted, err := h.evs.DiscoverVolume(ctx, opts)
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		again, err := h.evs.DiscoverVolume(ctx, opts)
		if err != nil {
			t.Fatalf("discover again: %v", err)
		}
		if adopted.ID != again.ID {
			t.Fatalf("two calls adopted different volumes: %s then %s", adopted.ID, again.ID)
		}
		h.evidence.Observe("duplicate_name_adopted", adopted.ID)
	})

	clause(t, catalogue.CheckDeletingNotAdopted, func(t *testing.T) {
		volume := putIntoDeleting(ctx, t, "vanishing")

		_, err := h.evs.DiscoverVolume(ctx, h.discoverOpts(volume.Name, minimumVolumeGiB))
		if !errors.Is(err, evs.ErrConflict) && !errors.Is(err, evs.ErrNotFound) {
			t.Fatalf("discovery adopted a disappearing volume: %v", err)
		}
	})

	clause(t, catalogue.CheckLaterPageRevealsCandidate, func(t *testing.T) {
		if h.env.pagingVolumes == 0 {
			notAuthorized(
				t,
				"reaching a later discovery page means provisioning more same-name volumes than"+
					" one page holds; set "+settings.EnvPagingVolumeCount+" to authorize that cost",
			)
		}
		pagingCandidates(ctx, t, h.env.pagingVolumes)
	})

	clause(t, catalogue.CheckNoCallerValueInTags, func(t *testing.T) {
		volume, err := h.createMarkedVolume(ctx, "no-reflection", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		detail, err := h.evs.GetVolume(ctx, volume.ID)
		if err != nil {
			t.Fatalf("read detail: %v", err)
		}

		if len(detail.Tags) != 1 {
			t.Fatalf("volume carries %d tags, not the ownership marker alone: %v",
				len(detail.Tags), detail.Tags)
		}
		if detail.Tags[evs.OwnershipTagKey] != evs.OwnershipTagValue {
			t.Fatalf("the single tag is not the ownership marker: %v", detail.Tags)
		}
		for key, value := range detail.Metadata {
			if value == volume.Name || value == h.env.volumeType || value == h.identity.Zone {
				t.Fatalf("metadata key %q reflects a caller value", key)
			}
		}
	})
}

// TestVolumeDeletion covers the deletion branches, each of which
// decides whether a destructive call is issued at all.
func TestVolumeDeletion(t *testing.T) {
	h := runState
	ctx := h.context()

	clause(t, catalogue.CheckUnmarkedDeletionRefused, func(t *testing.T) {
		id, err := h.createUnmarkedVolume(ctx, "undeletable", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		if err := h.evs.DeleteVolume(ctx, id); !errors.Is(err, evs.ErrNotOwned) {
			t.Fatalf("deletion answered %v, not a refusal to act on a foreign volume", err)
		}
		if _, err := h.getVolumeDirect(ctx, id); err != nil {
			t.Fatalf("the refused volume is no longer intact: %v", err)
		}
	})

	clause(t, catalogue.CheckAbsentDeletionReported, func(t *testing.T) {
		volume, err := h.createMarkedVolume(ctx, "absent", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := h.evs.DeleteVolume(ctx, volume.ID); err != nil {
			t.Fatalf("first deletion: %v", err)
		}
		if err := h.evs.DeleteVolume(ctx, volume.ID); !errors.Is(err, evs.ErrNotFound) {
			t.Fatalf("deleting an absent volume answered %v", err)
		}
	})

	clause(t, catalogue.CheckInFlightDeletionObserved, func(t *testing.T) {
		volume := putIntoDeleting(ctx, t, "in-progress")

		// A second destructive call against a volume in this state
		// is rejected input, so a successful answer is only
		// possible if the deletion was observed, not reissued.
		switch err := h.evs.DeleteVolume(ctx, volume.ID); {
		case err == nil:
			h.evidence.Observe("in_flight_deletion_observed", volume.ID)
		case errors.Is(err, evs.ErrNotFound):
			// Either the deletion finished within the round
			// trip that followed the observation or the
			// detail surface the driver reads hides a volume
			// the plain surface still reports. Only the
			// second is a finding, so ask the plain surface
			// which one happened.
			after, direct := h.getVolumeDirect(ctx, volume.ID)
			switch {
			case direct == nil:
				t.Fatalf(
					"the driver reported the volume absent while the plain surface still"+
						" reports status %q", after.Status)
			case !isAbsent(direct):
				t.Fatalf("reading the volume after its reported absence: %v", direct)
			}
			windowMissed(
				t,
				"the deletion completed before the driver could read the volume in flight",
			)
		default:
			t.Fatalf("observing an in-flight deletion answered %v", err)
		}
	})

	clause(t, catalogue.CheckFailedDeletionReported, func(t *testing.T) {
		notForceable(
			t,
			"neither the service nor a caller can put a volume into a failed-deletion state on"+
				" demand",
		)
	})

	clause(t, catalogue.CheckAbsenceNeverArrives, func(t *testing.T) {
		notForceable(
			t,
			"the service cannot be made to accept a deletion and then keep the volume present",
		)
	})
}

// discoverOpts builds the discovery request every adoption clause
// issues: same zone, same type and a caller-chosen minimum size, so
// only the candidate under test decides the answer.
func (h *harness) discoverOpts(name string, minSizeGiB int) evs.DiscoverVolumeOpts {
	return evs.DiscoverVolumeOpts{
		Name:             name,
		AvailabilityZone: h.identity.Zone,
		VolumeType:       h.env.volumeType,
		MinSizeGiB:       minSizeGiB,
	}
}

// discoverExpectConflict asserts discovery refuses the named candidate
// as a conflict.
func discoverExpectConflict(ctx context.Context, t *testing.T, name string, minSizeGiB int) {
	t.Helper()

	h := runState
	if _, err := h.evs.DiscoverVolume(ctx, h.discoverOpts(name, minSizeGiB)); !errors.Is(
		err,
		evs.ErrConflict,
	) {
		t.Fatalf("discovery answered %v, not a conflict", err)
	}
}

// putIntoDeleting creates a marked volume and catches it in the
// deleting state, which is the only moment the in-flight deletion
// branches can be observed.
func putIntoDeleting(ctx context.Context, t *testing.T, suffix string) *evs.Volume {
	t.Helper()

	h := runState
	volume, err := h.createMarkedVolume(ctx, suffix, minimumVolumeGiB)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := h.deleteVolumeDirectAsync(ctx, volume.ID); err != nil {
		t.Fatalf("request deletion: %v", err)
	}
	if !h.awaitStatusWindow(ctx, volume.ID, statusDeleting) {
		windowMissed(
			t,
			"the volume left the deleting state before the run could observe it there",
		)
	}
	return volume
}

// assertUnchanged confirms a candidate the driver refused was left
// exactly as it was found.
func assertUnchanged(ctx context.Context, t *testing.T, id, wantStatus string) {
	t.Helper()

	after, err := runState.getVolumeDirect(ctx, id)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if after.Status != wantStatus {
		t.Fatalf("the refused candidate now reports status %q, not %q", after.Status, wantStatus)
	}
	if len(after.Attachments) != 0 {
		t.Fatalf("the refused candidate gained %d attachment(s)", len(after.Attachments))
	}
}

// pagingCandidates provisions enough same-name volumes that one can
// only be reached on a later discovery page, then confirms discovery
// still adopts one of them.
func pagingCandidates(ctx context.Context, t *testing.T, count int) {
	t.Helper()

	h := runState
	volumeName := h.name("paged")
	for i := 0; i < count; i++ {
		if _, err := h.createUnmarkedVolumeNamed(ctx, volumeName, minimumVolumeGiB); err != nil {
			t.Fatalf("create candidate %d: %v", i, err)
		}
	}

	first, err := h.listDiscoveryPage(ctx, volumeName, 0)
	if err != nil {
		t.Fatalf("read first discovery page: %v", err)
	}
	if len(first) != evs.DiscoveryPageSize {
		windowMissed(t, fmt.Sprintf(
			"the service returned %d exact-name items on the first page, not %d",
			len(first),
			evs.DiscoveryPageSize,
		))
	}

	later, err := h.listDiscoveryPage(ctx, volumeName, evs.DiscoveryPageSize)
	if err != nil {
		t.Fatalf("read later discovery page: %v", err)
	}
	if len(later) == 0 {
		windowMissed(t, "the service did not keep an exact-name candidate beyond the first page")
	}
	candidate := later[0]
	if _, err := tags.Create(
		serviceClientWithContext(ctx, h.v2),
		"volumes",
		candidate.ID,
		tags.CreateOpts{Tags: map[string]string{evs.OwnershipTagKey: evs.OwnershipTagValue}},
	).Extract(); err != nil {
		t.Fatalf("mark later-page candidate: %v", err)
	}

	first, err = h.listDiscoveryPage(ctx, volumeName, 0)
	if err != nil {
		t.Fatalf("verify first discovery page: %v", err)
	}
	later, err = h.listDiscoveryPage(ctx, volumeName, evs.DiscoveryPageSize)
	if err != nil {
		t.Fatalf("verify later discovery page: %v", err)
	}
	if len(first) != evs.DiscoveryPageSize || len(later) == 0 || later[0].ID != candidate.ID {
		windowMissed(
			t,
			"service ordering changed before discovery could verify the later-page candidate",
		)
	}
	for _, volume := range first {
		if volume.Tags[evs.OwnershipTagKey] == evs.OwnershipTagValue {
			windowMissed(t, "the first discovery page contains an adoptable marked candidate")
		}
	}
	if later[0].Tags[evs.OwnershipTagKey] != evs.OwnershipTagValue {
		windowMissed(t, "the ownership marker did not propagate to the later discovery page")
	}

	adopted, err := h.evs.DiscoverVolume(ctx, h.discoverOpts(volumeName, minimumVolumeGiB))
	if err != nil {
		t.Fatalf("discovery across pages: %v", err)
	}
	if adopted.ID != candidate.ID {
		t.Fatalf(
			"discovery adopted %q, want verified later-page candidate %q",
			adopted.ID,
			candidate.ID,
		)
	}
	h.evidence.Observe("paged_candidate_adopted", adopted.ID)
	h.evidence.Observe("paged_candidate_count", strconv.Itoa(count))
	h.evidence.Observe("paged_candidate_offset", strconv.Itoa(evs.DiscoveryPageSize))
}

func (h *harness) listDiscoveryPage(
	ctx context.Context,
	volumeName string,
	offset int,
) ([]cloudvolumes.Volume, error) {
	listed, err := cloudvolumes.List(serviceClientWithContext(ctx, h.v2), cloudvolumes.ListOpts{
		Name:   volumeName,
		Limit:  evs.DiscoveryPageSize,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	exact := make([]cloudvolumes.Volume, 0, len(listed))
	for _, volume := range listed {
		if volume.Name == volumeName {
			exact = append(exact, volume)
		}
	}
	return exact, nil
}

// awaitStatusWindow watches for a short-lived status and reports
// whether the run caught it.
func (h *harness) awaitStatusWindow(ctx context.Context, id, want string) bool {
	_, err := poll(ctx, time.Second, fixturePollTimeout,
		func(ctx context.Context) (struct{}, bool, error) {
			volume, err := h.getVolumeDirect(ctx, id)
			if err != nil {
				return struct{}{}, false, err
			}
			return struct{}{}, volume.Status == want, nil
		})
	if err != nil {
		return false
	}
	h.evidence.Observe("observed_status_"+want, id)
	return true
}
