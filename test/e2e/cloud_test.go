//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	blockstoragev3 "github.com/opentelekomcloud/gophertelekomcloud/openstack/blockstorage/v3/volumes"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/reclaim"
)

const (
	// minimumVolumeGiB is the smallest data volume the service accepts,
	// and therefore the size every fixture uses unless a scenario needs
	// a mismatch.
	minimumVolumeGiB = 10

	fixturePollInterval = 2 * time.Second
	fixturePollTimeout  = 5 * time.Minute

	// settlePollInterval and settleTimeout bound a wait a scenario takes while deciding what to do
	// next. These are attachment-scale, not fixture-scale.
	settlePollInterval = 500 * time.Millisecond
	settleTimeout      = 20 * time.Second

	// inFlightTimeout bounds the shorter question of whether the service
	// took an effect at all. A scenario asks it repeatedly while looking
	// for a timing window, so waiting as long as a settle would spend
	// most of the scenario on attempts that already answered no.
	inFlightTimeout = 8 * time.Second
)

// Volume states this run reads back from the service. They are spelled
// here because the run observes the service directly and the driver
// keeps its own copies unexported.
const (
	statusCreating  = "creating"
	statusAvailable = "available"
	statusAttaching = "attaching"
	statusInUse     = "in-use"
	statusDetaching = "detaching"
	statusDeleting  = "deleting"
)

// isAbsent reports a cloud answer that the resource does not exist.
func isAbsent(err error) bool {
	if errors.Is(err, evs.ErrNotFound) {
		return true
	}
	var notFound golangsdk.ErrDefault404
	return errors.As(err, &notFound)
}

// name builds a run-scoped resource name. Every resource a run creates
// carries the run prefix, so a sweep can find it whether or not the
// driver marked it as its own.
func (h *harness) name(suffix string) string {
	return h.prefix + suffix
}

// createUnmarkedVolume provisions a volume the driver did not create, so the refusals that gate on
// the ownership marker have something real to refuse. It uses the plain volume API. The driver's
// own boundary applies the marker and has no way to withhold it.
func (h *harness) createUnmarkedVolume(ctx context.Context, suffix string, sizeGiB int) (
	string,
	error,
) {
	return h.createUnmarkedVolumeNamed(ctx, h.name(suffix), sizeGiB)
}

func (h *harness) createUnmarkedVolumeNamed(ctx context.Context, volumeName string, sizeGiB int) (
	string,
	error,
) {
	if err := h.ledger.record(
		reclaim.Entry{Kind: reclaim.ResourceVolume, Name: volumeName},
	); err != nil {
		return "", err
	}

	created, err := blockstoragev3.Create(serviceClientWithContext(ctx, h.v3), blockstoragev3.CreateOpts{
		Name:             volumeName,
		Size:             sizeGiB,
		AvailabilityZone: h.identity.Zone,
		VolumeType:       h.env.volumeType,
	}).
		Extract()
	if err != nil {
		return "", fmt.Errorf("create unmarked volume: %w", err)
	}

	if err := h.ledger.recordVolumeID(volumeName, created.ID); err != nil {
		return created.ID, err
	}
	if err := h.awaitVolumeStatus(ctx, created.ID, statusAvailable); err != nil {
		return created.ID, err
	}
	return created.ID, nil
}

// createMarkedVolume provisions a volume through the driver's own
// boundary, which applies the ownership marker. suffix names it within
// the run. Its size can be set to disagree with a later request.
func (h *harness) createMarkedVolume(
	ctx context.Context,
	suffix string,
	sizeGiB int,
) (*evs.Volume, error) {
	return h.markedVolume(ctx, h.name(suffix), sizeGiB, "create marked volume")
}

// createMarkedVolumeNamed provisions a second volume under an existing
// run-scoped name, which is how a duplicate same-name candidate is
// produced.
func (h *harness) createMarkedVolumeNamed(
	ctx context.Context,
	volumeName string,
	sizeGiB int,
) (*evs.Volume, error) {
	return h.markedVolume(ctx, volumeName, sizeGiB, "create duplicate marked volume")
}

func (h *harness) markedVolume(
	ctx context.Context,
	volumeName string,
	sizeGiB int,
	label string,
) (*evs.Volume, error) {
	if err := h.ledger.record(
		reclaim.Entry{Kind: reclaim.ResourceVolume, Name: volumeName},
	); err != nil {
		return nil, err
	}

	volume, err := h.evs.CreateVolume(ctx, evs.CreateVolumeOpts{
		Name:             volumeName,
		Size:             sizeGiB,
		AvailabilityZone: h.identity.Zone,
		VolumeType:       h.env.volumeType,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if err := h.ledger.recordVolumeID(volumeName, volume.ID); err != nil {
		return volume, err
	}
	return volume, nil
}

// getVolumeDirect reads a volume through the plain volume API, so a
// scenario can confirm a candidate was left untouched without asking
// the code it is testing.
func (h *harness) getVolumeDirect(ctx context.Context, id string) (*blockstoragev3.Volume, error) {
	volume, err := blockstoragev3.Get(serviceClientWithContext(ctx, h.v3), id).Extract()
	if err != nil {
		return nil, err
	}
	return volume, nil
}

// deleteVolumeDirect removes a volume regardless of its marker and waits
// until it is gone. Teardown uses it because a run creates volumes the
// driver refuses to delete.
func (h *harness) deleteVolumeDirect(ctx context.Context, id string) error {
	if err := h.requestVolumeDeletion(ctx, id, "delete volume"); err != nil {
		return err
	}
	return h.awaitVolumeAbsence(ctx, id)
}

// deleteVolumeDirectAsync issues the destructive call and returns
// without waiting, so a scenario can observe the transient state the
// service passes through.
func (h *harness) deleteVolumeDirectAsync(ctx context.Context, id string) error {
	return h.requestVolumeDeletion(ctx, id, "request deletion")
}

func (h *harness) requestVolumeDeletion(ctx context.Context, id, label string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := blockstoragev3.Delete(serviceClientWithContext(ctx, h.v3), id).
		ExtractErr(); err != nil &&
		!isAbsent(err) {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// detachAndDelete releases an attachment this run may have created
// before reclaiming the volume.
func (h *harness) detachAndDelete(ctx context.Context, id string) error {
	if _, err := h.getVolumeDirect(ctx, id); err != nil {
		if isAbsent(err) {
			return nil
		}
		return fmt.Errorf("inspect volume before reclaim: %w", err)
	}

	// A volume the service is still moving accepts neither a detach nor
	// a delete. A scenario that left one mid-transition is exactly the
	// case reclamation exists for.
	h.awaitSettledVolume(ctx, id)

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.evs.DetachVolume(ctx, id, h.identity.ServerID); err != nil &&
		!errors.Is(err, evs.ErrNotFound) {
		return fmt.Errorf("detach before reclaim: %w", err)
	}
	// Detaching returns once the attachment is gone from the instance,
	// which the volume's own state can still lag behind. A delete issued
	// against that lag is refused.
	h.awaitSettledVolume(ctx, id)

	if err := ctx.Err(); err != nil {
		return err
	}
	return h.deleteVolumeDirect(ctx, id)
}

// transitionalStatuses are the states the service passes through on its
// own. A volume reporting one accepts no new request about its
// attachment, so waiting is the only thing a caller can do.
var transitionalStatuses = []string{statusCreating, statusAttaching, statusDetaching}

// errPollTimeout says the awaited condition never arrived inside its
// budget, so a caller can tell running out of time apart from a
// terminal answer.
var errPollTimeout = errors.New("the awaited condition did not arrive in time")

// poll repeats attempt until it reports done, answers a terminal error,
// runs past timeout or the context ends. It always returns the last
// value attempt produced, so a caller can say what it saw.
func poll[T any](
	ctx context.Context,
	interval, timeout time.Duration,
	attempt func(context.Context) (T, bool, error),
) (T, error) {
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(interval)
	defer timer.Stop()

	var last T
	for {
		value, done, err := attempt(ctx)
		last = value
		if err != nil || done {
			return last, err
		}
		if time.Now().After(deadline) {
			return last, errPollTimeout
		}

		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-timer.C:
			timer.Reset(interval)
		}
	}
}

// awaitSettledVolume waits for the volume to leave a transitional state and reports whether it
// settled. If it does not settle inside the budget, the caller decides what to do next.
func (h *harness) awaitSettledVolume(ctx context.Context, id string) bool {
	_, err := poll(ctx, settlePollInterval, settleTimeout,
		func(ctx context.Context) (struct{}, bool, error) {
			volume, err := h.getVolumeDirect(ctx, id)
			if err != nil {
				return struct{}{}, false, err
			}
			return struct{}{}, !slices.Contains(transitionalStatuses, volume.Status), nil
		})
	return err == nil
}

// awaitAttachInFlight reports whether the service accepted an attach for this volume. The wait is
// short: callers use the answer to pick the next step, not to wait out a fixture.
func (h *harness) awaitAttachInFlight(ctx context.Context, id string) (string, bool) {
	status, err := poll(ctx, settlePollInterval, inFlightTimeout,
		func(ctx context.Context) (string, bool, error) {
			volume, err := h.getVolumeDirect(ctx, id)
			if err != nil {
				return "", false, err
			}
			inFlight := volume.Status == statusAttaching || volume.Status == statusInUse
			return volume.Status, inFlight, nil
		})
	return status, err == nil
}

func (h *harness) awaitVolumeStatus(ctx context.Context, id, want string) error {
	got, err := poll(ctx, fixturePollInterval, fixturePollTimeout,
		func(ctx context.Context) (string, bool, error) {
			volume, err := h.getVolumeDirect(ctx, id)
			if err != nil {
				return "", false, fmt.Errorf("await status %s: %w", want, err)
			}
			return volume.Status, volume.Status == want, nil
		})
	if errors.Is(err, errPollTimeout) {
		return fmt.Errorf("volume %s reported %s, not %s", id, got, want)
	}
	return err
}

func (h *harness) awaitVolumeAbsence(ctx context.Context, id string) error {
	// A read that fails without saying the volume is gone is not an
	// answer, so it keeps polling.
	_, err := poll(ctx, fixturePollInterval, fixturePollTimeout,
		func(ctx context.Context) (struct{}, bool, error) {
			_, err := h.getVolumeDirect(ctx, id)
			return struct{}{}, isAbsent(err), nil
		})
	if errors.Is(err, errPollTimeout) {
		return fmt.Errorf("volume %s is still present", id)
	}
	return err
}

// findVolumeIDsByName resolves every exact-name match for a create call
// that never reported an identifier. Ambiguous same-name fixtures make
// a single-result lookup unsafe for reclamation.
func (h *harness) findVolumeIDsByName(ctx context.Context, volumeName string) ([]string, error) {
	volumes, err := h.evs.ListVolumes(ctx, evs.ListVolumeOpts{Name: volumeName})
	if err != nil {
		if isAbsent(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for i := range volumes {
		if volumes[i].Name == volumeName {
			ids = append(ids, volumes[i].ID)
		}
	}
	return ids, nil
}

// listVolumesWithPrefix returns every volume in the project whose name
// opens with prefix.
func (h *harness) listVolumesWithPrefix(ctx context.Context, prefix string) ([]evs.Volume, error) {
	volumes, err := h.evs.ListVolumes(ctx, evs.ListVolumeOpts{})
	if err != nil {
		return nil, err
	}

	var matched []evs.Volume
	for i := range volumes {
		if strings.HasPrefix(volumes[i].Name, prefix) {
			matched = append(matched, volumes[i])
		}
	}
	return matched, nil
}

// snapshotMarkedVolumes records the driver-marked volumes that existed
// before the run, so teardown can refuse to touch them even if one
// shares a name with a fixture.
func (h *harness) snapshotMarkedVolumes(ctx context.Context) (map[string]bool, error) {
	volumes, err := h.evs.ListVolumes(ctx, evs.ListVolumeOpts{})
	if err != nil {
		return nil, fmt.Errorf("snapshot existing volumes: %w", err)
	}

	existing := map[string]bool{}
	for i := range volumes {
		if volumes[i].Tags[evs.OwnershipTagKey] == evs.OwnershipTagValue {
			existing[volumes[i].ID] = true
		}
	}
	return existing, nil
}
