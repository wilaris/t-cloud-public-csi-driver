//go:build e2e

package e2e

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
)

// attachCancellationDelays are timeouts used to catch an attach that has been sent but not yet
// answered. Too short and the request never left; too long and the attach already finished. The
// list steps up until one overshoots, which brackets the window.
var attachCancellationDelays = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// TestAttachmentHandoff drives the attachment boundary against the real compute and volume APIs
// and confirms the guest sees what the cloud reported.
func TestAttachmentHandoff(t *testing.T) {
	h := runState
	ctx := h.context()

	volume, err := h.createMarkedVolume(ctx, "attach", minimumVolumeGiB)
	if err != nil {
		t.Fatalf("create volume to attach: %v", err)
	}
	h.reserveAttachment(t, volume.Name, volume.ID)

	var attachment *evs.Attachment

	clause(t, catalogue.CheckAttachmentNamesThisServer, func(t *testing.T) {
		observed, err := h.evs.AttachVolume(ctx, volume.ID, h.identity.ServerID)
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		attachment = observed

		if observed.ServerID != h.identity.ServerID {
			t.Fatalf("attachment names server %s, not the instance running this", observed.ServerID)
		}
		if observed.VolumeID != volume.ID {
			t.Fatalf("attachment names volume %s, not the one attached", observed.VolumeID)
		}
		if strings.TrimSpace(observed.DeviceName) == "" {
			t.Fatalf("attachment reported success without a device name")
		}
		h.evidence.Observe("attached_volume_id", observed.VolumeID)
		h.evidence.Observe("attached_device_name", observed.DeviceName)
	})

	// Later checks need this attachment.
	// Each one skips itself if the attach never produced a descriptor.
	noAttachment := "the attaching check produced no descriptor to observe"

	t.Cleanup(func() {
		// The attach may never have taken effect, so an absent
		// attachment is not a teardown failure.
		err := h.evs.DetachVolume(ctx, volume.ID, h.identity.ServerID)
		if err != nil && !isAbsent(err) {
			t.Errorf("detach: %v", err)
		}
	})

	clause(t, catalogue.CheckAttachedStateExplained, func(t *testing.T) {
		if attachment == nil {
			blocked(t, noAttachment)
		}
		detail, err := h.evs.GetVolume(ctx, volume.ID)
		if err != nil {
			t.Fatalf("read detail: %v", err)
		}
		if detail.Status != statusInUse {
			t.Fatalf("an attached volume reports status %q", detail.Status)
		}
		h.evidence.Observe("attached_volume_status", detail.Status)
	})

	clause(t, catalogue.CheckGuestExposesNamedDevice, func(t *testing.T) {
		if attachment == nil {
			blocked(t, noAttachment)
		}
		block, err := isBlockDevice(attachment.DeviceName)
		if err != nil {
			t.Fatalf("the guest does not expose %s: %v", attachment.DeviceName, err)
		}
		if !block {
			t.Fatalf("%s is not a block device in this guest", attachment.DeviceName)
		}
	})

	clause(t, catalogue.CheckStableLinkAgrees, func(t *testing.T) {
		if attachment == nil {
			blocked(t, noAttachment)
		}
		links, err := byIDLinksFor(volume.ID)
		if err != nil {
			t.Fatalf("read stable links: %v", err)
		}
		if len(links) == 0 {
			t.Fatalf("no stable link names volume %s", volume.ID)
		}
		h.evidence.Observe("by_id_links", strings.Join(links, " "))

		for _, link := range links {
			same, err := sameKernelDevice(link, attachment.DeviceName)
			if err != nil {
				t.Fatalf("compare %s with %s: %v", link, attachment.DeviceName, err)
			}
			if !same {
				t.Fatalf("%s and %s name different kernel devices", link, attachment.DeviceName)
			}
		}
	})

	clause(t, catalogue.CheckBothBusNamings, func(t *testing.T) {
		if attachment == nil {
			blocked(t, noAttachment)
		}
		links, err := byIDLinksFor(volume.ID)
		if err != nil {
			t.Fatalf("read stable links: %v", err)
		}

		var sawVirtio, sawSCSI bool
		for _, link := range links {
			base := link[strings.LastIndex(link, "/")+1:]
			sawVirtio = sawVirtio || strings.HasPrefix(base, "virtio-")
			sawSCSI = sawSCSI || strings.HasPrefix(base, "scsi-")
		}
		if !sawVirtio || !sawSCSI {
			shapeLimited(
				t,
				"this instance exposes one disk bus, so a single run cannot observe both namings",
			)
		}
	})

	clause(t, catalogue.CheckRepeatedAttachIdempotent, func(t *testing.T) {
		if attachment == nil {
			blocked(t, noAttachment)
		}
		again, err := h.evs.AttachVolume(ctx, volume.ID, h.identity.ServerID)
		if err != nil {
			t.Fatalf("attach again: %v", err)
		}
		if again.DeviceName != attachment.DeviceName || again.VolumeID != attachment.VolumeID ||
			again.ServerID != attachment.ServerID {
			t.Fatalf("the idempotent path answered %+v, not %+v", again, attachment)
		}
	})
}

// TestAttachmentCancellation covers canceling an in-flight attach against the real service,
// including the two clauses a run has to induce.
func TestAttachmentCancellation(t *testing.T) {
	h := runState
	ctx := h.context()

	clause(t, catalogue.CheckClassifiedFailureNoSecret, func(t *testing.T) {
		_, err := h.evs.GetVolume(ctx, "00000000-0000-0000-0000-000000000000")
		if err == nil {
			t.Fatalf("reading a volume that cannot exist succeeded")
		}
		if !errors.Is(err, evs.ErrNotFound) && !errors.Is(err, evs.ErrInvalidArgument) {
			t.Fatalf("the failure matched no category: %v", err)
		}
		if err := h.env.assertContained("classified failure", err.Error()); err != nil {
			t.Fatalf("%v", err)
		}
	})

	clause(t, catalogue.CheckCancellationReachesWire, func(t *testing.T) {
		volume, err := h.createMarkedVolume(ctx, "cancel", minimumVolumeGiB)
		if err != nil {
			t.Fatalf("create volume to attach: %v", err)
		}
		h.reserveAttachment(t, volume.Name, volume.ID)
		t.Cleanup(func() {
			// A volume the service is still moving accepts no detach and the canceled attach
			// may have left nothing to detach at all, so absence is an expected answer too.
			h.awaitSettledVolume(ctx, volume.ID)
			err := h.evs.DetachVolume(ctx, volume.ID, h.identity.ServerID)
			if err != nil && !isAbsent(err) {
				t.Errorf("detach: %v", err)
			}
		})

		// An attach issued against a volume the service is still creating is rejected as "still creating"
		// and never reaches the wire. A creating volume can also look like one being attached.
		if err := h.awaitVolumeStatus(ctx, volume.ID, statusAvailable); err != nil {
			t.Fatalf("the volume to attach never became attachable: %v", err)
		}

		delay, status, induced := h.induceAttachAmbiguity(ctx, t, volume.ID)
		h.evidence.Observe("cancelled_attach_volume", volume.ID)
		if !induced {
			windowMissed(
				t,
				"no delay this run tried left the service holding an attach the cancelled caller"+
					" never saw, so the reconciliation path was never entered",
			)
		}
		h.evidence.Observe("induction_delay_ms", strconv.FormatInt(delay.Milliseconds(), 10))
		h.evidence.Observe("post_cancellation_status", status)

		reissued, err := h.evs.AttachVolume(ctx, volume.ID, h.identity.ServerID)
		if err != nil {
			t.Fatalf("re-issuing after cancellation: %v", err)
		}
		if strings.TrimSpace(reissued.DeviceName) == "" {
			t.Fatalf("the re-issued attach reported success without a device name")
		}
		h.evidence.Observe("ambiguous_attach_device", reissued.DeviceName)
	})
}

// cancellationOutcome says what one canceled attach produced.
// That result decides whether the next attempt needs more or less time on the wire.
type cancellationOutcome int

const (
	// cancelledTooEarly means the service held nothing when the caller gave up.
	cancelledTooEarly cancellationOutcome = iota
	// cancelledMidFlight means the service accepted an attach the canceled caller never saw, which is
	// the state the check exists to observe.
	cancelledMidFlight
	// cancelledTooLate means the attach finished inside the window, so
	// nothing about it is ambiguous.
	cancelledTooLate
)

// attachCancellationBisections bounds the narrowing below. Each halving needs a real attach against
// the service, so the schedule trades wall clock for precision and then stops.
const attachCancellationBisections = 3

// induceAttachAmbiguity cancels an attach while it is on the wire and reports the delay that left
// the service holding an effect the caller never saw. It walks the schedule upward until an attempt
// lands in that window or overshoots it, then halves the bracket. A completed attach and an
// untouched volume bound the window from opposite sides.
func (h *harness) induceAttachAmbiguity(
	ctx context.Context,
	t *testing.T,
	volumeID string,
) (time.Duration, string, bool) {
	var tooShort, tooLong time.Duration

walk:
	for _, delay := range attachCancellationDelays {
		outcome, status := h.attemptCancelledAttach(ctx, t, volumeID, delay)
		switch outcome {
		case cancelledMidFlight:
			return delay, status, true
		case cancelledTooEarly:
			tooShort = delay
		case cancelledTooLate:
			// Every longer delay would complete too, so the
			// window is below this one.
			tooLong = delay
			h.releaseCompletedAttach(ctx, t, volumeID)
			break walk
		}
	}

	if tooLong == 0 {
		// The longest delay in the schedule still reached nothing,
		// so this run never bracketed the window and has no
		// interval to narrow.
		return 0, "", false
	}

	for range attachCancellationBisections {
		delay := tooShort + (tooLong-tooShort)/2
		outcome, status := h.attemptCancelledAttach(ctx, t, volumeID, delay)
		switch outcome {
		case cancelledMidFlight:
			return delay, status, true
		case cancelledTooEarly:
			tooShort = delay
		case cancelledTooLate:
			tooLong = delay
			h.releaseCompletedAttach(ctx, t, volumeID)
		}
	}
	return 0, "", false
}

// attemptCancelledAttach issues one attach bounded by delay and reads
// back what the service kept.
func (h *harness) attemptCancelledAttach(
	ctx context.Context,
	t *testing.T,
	volumeID string,
	delay time.Duration,
) (cancellationOutcome, string) {
	t.Helper()

	cancelCtx, cancel := context.WithTimeout(ctx, delay)
	defer cancel()

	if _, err := h.evs.AttachVolume(cancelCtx, volumeID, h.identity.ServerID); err == nil {
		return cancelledTooLate, ""
	} else if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled attach answered %v, not its context cause", err)
	}

	if status, inFlight := h.awaitAttachInFlight(ctx, volumeID); inFlight {
		return cancelledMidFlight, status
	}
	return cancelledTooEarly, ""
}

// releaseCompletedAttach puts the volume back where the next attempt
// needs it, because an attach that already finished leaves nothing to
// cancel.
func (h *harness) releaseCompletedAttach(ctx context.Context, t *testing.T, volumeID string) {
	t.Helper()

	if err := h.evs.DetachVolume(ctx, volumeID, h.identity.ServerID); err != nil {
		t.Fatalf("release a completed attach before trying a shorter delay: %v", err)
	}
	if err := h.awaitVolumeStatus(ctx, volumeID, statusAvailable); err != nil {
		t.Fatalf("the released volume did not become attachable again: %v", err)
	}
}
