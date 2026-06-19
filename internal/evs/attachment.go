package evs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/ecs/v1/cloudservers"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/ecs/v1/disk"
	v3volumes "github.com/opentelekomcloud/gophertelekomcloud/openstack/evs/v3/volumes"
)

const attachmentPollInterval = time.Second

// refusedAttachReconcileWindow bounds the wait after compute rejects an attach that the volume
// record shows as accepted. It is shorter than the operation bound so a wrong probe reading is
// reported in seconds, not minutes.
const refusedAttachReconcileWindow = 90 * time.Second

// refusedDetachReconcileWindow bounds the wait after compute rejects a detach that the volume
// record shows as moving. It is shorter than the operation bound so a wrong probe reading is
// reported in seconds, not minutes.
const refusedDetachReconcileWindow = 90 * time.Second

// detachKeepVolume is the disk.Detach delete flag that leaves the volume in place. Any other
// value would delete the volume with the attachment.
const detachKeepVolume = 0

// Attachment is one observed EVS volume attachment on a compute instance.
// DeviceName must be set; the node locates the disk from that path.
type Attachment struct {
	VolumeID   string
	ServerID   string
	DeviceName string
}

// AttachVolume attaches volumeID to serverID and returns the settled attachment.
// An already-attached volume with a device name is returned without another attach call.
func (c *Client) AttachVolume(
	ctx context.Context,
	volumeID, serverID string,
) (*Attachment, error) {
	operation := fmt.Sprintf("attach volume %s to server %s", volumeID, serverID)
	if err := validateAttachmentInput(volumeID, serverID); err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}

	ctx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()

	existing, err := c.observeAttachment(ctx, c.ecs(ctx), volumeID, serverID)
	if err != nil {
		return nil, c.classifyError(operation, err)
	}
	if attachedWithDevice(existing) {
		return existing, nil
	}
	if existing != nil {
		attached, err := c.waitForAttachmentState(ctx, volumeID, serverID, attachedWithDevice)
		if err != nil {
			return nil, c.classifyError(operation, err)
		}
		return attached, nil
	}

	job, err := disk.Attach(c.ecs(ctx), disk.CreateOpts{
		ServerID: serverID,
		VolumeAttachment: &disk.VolumeAttachment{
			VolumeID: volumeID,
		},
	})
	if err != nil {
		return c.finishAttachAfterEffectError(ctx, operation, volumeID, serverID, err)
	}
	if job == nil || strings.TrimSpace(job.JobID) == "" {
		attached, err := c.waitForAttachmentState(ctx, volumeID, serverID, attachedWithDevice)
		if err == nil {
			return attached, nil
		}
		if ctx.Err() != nil {
			return nil, c.classifyError(operation, ctx.Err())
		}
		return nil, fmt.Errorf("%s: %w", operation, ErrOperationFailed)
	}

	err = c.waitForAttachmentJob(ctx, job.JobID)
	if err != nil {
		return nil, c.classifyError(operation, err)
	}
	attached, err := c.waitForAttachmentState(ctx, volumeID, serverID, attachedWithDevice)
	if err != nil {
		return nil, c.classifyError(operation, err)
	}
	return attached, nil
}

// DetachVolume detaches volumeID from the compute instance identified by serverID.
func (c *Client) DetachVolume(ctx context.Context, volumeID, serverID string) error {
	operation := fmt.Sprintf("detach volume %s from server %s", volumeID, serverID)
	if err := validateAttachmentInput(volumeID, serverID); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}

	ctx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()

	existing, err := c.observeAttachment(ctx, c.ecs(ctx), volumeID, serverID)
	if err != nil {
		return c.classifyError(operation, err)
	}
	if detached(existing) {
		return nil
	}

	job, err := disk.Detach(c.ecs(ctx), serverID, volumeID, detachKeepVolume)
	if err != nil {
		return c.finishDetachAfterEffectError(ctx, operation, volumeID, serverID, err)
	}
	if job == nil || strings.TrimSpace(job.JobID) == "" {
		_, err = c.waitForAttachmentState(ctx, volumeID, serverID, detached)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return c.classifyError(operation, ctx.Err())
		}
		return fmt.Errorf("%s: %w", operation, ErrOperationFailed)
	}

	err = c.waitForAttachmentJob(ctx, job.JobID)
	if err != nil {
		return c.classifyError(operation, err)
	}
	_, err = c.waitForAttachmentState(ctx, volumeID, serverID, detached)
	if err != nil {
		return c.classifyError(operation, err)
	}
	return nil
}

func attachedWithDevice(attachment *Attachment) bool {
	return attachment != nil && attachment.DeviceName != ""
}

func detached(attachment *Attachment) bool {
	return attachment == nil
}

func validateAttachmentInput(volumeID, serverID string) error {
	if strings.TrimSpace(volumeID) == "" || strings.TrimSpace(serverID) == "" {
		return ErrInvalidArgument
	}
	return nil
}

// observeAttachment returns the matching compute attachment, or nil when that pair is absent.
// The caller reuses ecsClient across polls so each tick does not rebuild the service client.
func (c *Client) observeAttachment(
	ctx context.Context,
	ecsClient *golangsdk.ServiceClient,
	volumeID, serverID string,
) (*Attachment, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	attachments, err := disk.GetAttachments(ecsClient, serverID)
	if err != nil {
		return nil, err
	}
	if attachments == nil {
		return nil, nil
	}

	for _, attachment := range attachments.VolumeAttachments {
		if attachment.VolumeID != volumeID || attachment.ServerID != serverID {
			continue
		}
		return &Attachment{
			VolumeID:   attachment.VolumeID,
			ServerID:   attachment.ServerID,
			DeviceName: strings.TrimSpace(attachment.Device),
		}, nil
	}
	return nil, nil
}

// refusalProbeReading is what the volume record shows after compute refused an attach.
type refusalProbeReading int

const (
	probeUnknown refusalProbeReading = iota
	// probeDenied: the volume record shows no attach, so the refusal stands.
	probeDenied
	// probeInFlight: the volume record already shows the requested attach; keep waiting.
	probeInFlight
	// probeAttachedElsewhere: another server holds the volume; report a conflict.
	probeAttachedElsewhere
)

// attachInFlight reads the volume record after compute refuses the attach. The compute listing
// omits an attachment that has been accepted but not settled, so the volume record is the only
// way to tell an in-flight attach from a rejected request.
func (c *Client) attachInFlight(
	ctx context.Context,
	volumeID, serverID string,
) (refusalProbeReading, error) {
	if ctx.Err() != nil {
		return probeUnknown, ctx.Err()
	}

	detail, err := v3volumes.Get(c.v3(ctx), volumeID).Extract()
	if err != nil {
		return probeUnknown, err
	}

	for _, attachment := range detail.Attachments {
		if attachment.ServerID == serverID {
			return probeInFlight, nil
		}
	}
	// A mid-attach volume may not list the pair yet. An empty list with status attaching counts
	// as the requested server; a list that already names another server does not.
	if len(detail.Attachments) > 0 {
		return probeAttachedElsewhere, nil
	}
	if detail.Status == volumeStatusAttaching {
		return probeInFlight, nil
	}
	return probeDenied, nil
}

// reconcileRefusedAttach waits, inside its own window, for the attachment the refusal implied.
// The window is independent of the operation bound so a wrong in-flight reading fails fast.
func (c *Client) reconcileRefusedAttach(
	ctx context.Context,
	volumeID, serverID string,
) (*Attachment, error) {
	reconcileCtx, cancel := context.WithTimeout(ctx, refusedAttachReconcileWindow)
	defer cancel()

	return c.waitForAttachmentState(reconcileCtx, volumeID, serverID, attachedWithDevice)
}

// finishAttachAfterEffectError classifies a failed attach call. Transient failures and an
// InvalidArgument backed by the volume record are reconciled; anything else is returned as the
// original classified error.
func (c *Client) finishAttachAfterEffectError(
	ctx context.Context,
	operation, volumeID, serverID string,
	attachErr error,
) (*Attachment, error) {
	kind := classifyErrorKind(attachErr)
	if errors.Is(kind, ErrUnavailable) || errors.Is(kind, ErrOperationFailed) {
		attached, observeErr := c.waitForAttachmentState(
			ctx,
			volumeID,
			serverID,
			attachedWithDevice,
		)
		if observeErr == nil {
			return attached, nil
		}
		if ctx.Err() != nil {
			return nil, c.classifyError(operation, ctx.Err())
		}
		return nil, c.classifyError(operation, attachErr)
	}
	if !errors.Is(kind, ErrInvalidArgument) {
		return nil, c.classifyError(operation, attachErr)
	}

	reading, probeErr := c.attachInFlight(ctx, volumeID, serverID)
	if ctx.Err() != nil {
		return nil, c.classifyError(operation, ctx.Err())
	}
	if probeErr != nil {
		return nil, c.classifyError(operation, attachErr)
	}
	if reading == probeAttachedElsewhere {
		return nil, fmt.Errorf(
			"%s: volume attached to another server: %w",
			operation,
			ErrConflict,
		)
	}
	if reading != probeInFlight {
		return nil, c.classifyError(operation, attachErr)
	}

	attached, observeErr := c.reconcileRefusedAttach(ctx, volumeID, serverID)
	if observeErr == nil {
		return attached, nil
	}
	if ctx.Err() != nil {
		return nil, c.classifyError(operation, ctx.Err())
	}
	return nil, c.classifyError(operation, attachErr)
}

// detachInFlight reads the volume record after compute refuses the detach. When the volume
// record shows the volume is still moving (attaching or detaching) or still attached to the
// server, detachment can be reconciled by waiting for the detached state.
func (c *Client) detachInFlight(
	ctx context.Context,
	volumeID, serverID string,
) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	detail, err := v3volumes.Get(c.v3(ctx), volumeID).Extract()
	if err != nil {
		return false, err
	}

	if detail.Status == volumeStatusAttaching || detail.Status == volumeStatusDetaching {
		return true, nil
	}

	for _, attachment := range detail.Attachments {
		if attachment.ServerID == serverID {
			return true, nil
		}
	}

	return false, nil
}

// reconcileRefusedDetach waits, inside its own window, for the volume to become detached.
// The window is independent of the operation bound so a wrong in-flight reading fails fast.
func (c *Client) reconcileRefusedDetach(
	ctx context.Context,
	volumeID, serverID string,
) error {
	reconcileCtx, cancel := context.WithTimeout(ctx, refusedDetachReconcileWindow)
	defer cancel()

	_, err := c.waitForAttachmentState(reconcileCtx, volumeID, serverID, detached)
	return err
}

// finishDetachAfterEffectError classifies a failed detach call. Transient failures and an
// InvalidArgument backed by the volume record are reconciled; anything else is returned as the
// original classified error.
func (c *Client) finishDetachAfterEffectError(
	ctx context.Context,
	operation, volumeID, serverID string,
	detachErr error,
) error {
	kind := classifyErrorKind(detachErr)
	if errors.Is(kind, ErrUnavailable) || errors.Is(kind, ErrOperationFailed) {
		_, observeErr := c.waitForAttachmentState(
			ctx,
			volumeID,
			serverID,
			detached,
		)
		if observeErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return c.classifyError(operation, ctx.Err())
		}
		return c.classifyError(operation, detachErr)
	}
	if !errors.Is(kind, ErrInvalidArgument) {
		return c.classifyError(operation, detachErr)
	}

	moving, probeErr := c.detachInFlight(ctx, volumeID, serverID)
	if ctx.Err() != nil {
		return c.classifyError(operation, ctx.Err())
	}
	if probeErr != nil {
		return c.classifyError(operation, detachErr)
	}
	if !moving {
		return c.classifyError(operation, detachErr)
	}

	observeErr := c.reconcileRefusedDetach(ctx, volumeID, serverID)
	if observeErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return c.classifyError(operation, ctx.Err())
	}
	return c.classifyError(operation, detachErr)
}

// waitForAttachmentJob polls job status itself because the SDK helper ignores caller
// cancellation and starts a goroutine per poll.
func (c *Client) waitForAttachmentJob(ctx context.Context, jobID string) error {
	jobClient := c.ecs(ctx)

	ticker := time.NewTicker(attachmentPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		job := new(cloudservers.JobStatus)
		//nolint:bodyclose // the SDK closes the response body when it decodes into the target
		_, err := jobClient.Get(jobClient.ServiceURL("jobs", jobID), &job, nil)
		if err == nil {
			if job.Status == "SUCCESS" {
				return nil
			}
			if job.Status == "FAIL" {
				return fmt.Errorf(
					"ECS job %s failed (code %s): %s",
					jobID,
					job.ErrorCode,
					job.FailReason,
				)
			}
		} else if jobPollShouldStop(err) {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitForAttachmentState polls until settled accepts the observation or the context expires.
func (c *Client) waitForAttachmentState(
	ctx context.Context,
	volumeID, serverID string,
	settled func(*Attachment) bool,
) (*Attachment, error) {
	ecsClient := c.ecs(ctx)

	ticker := time.NewTicker(attachmentPollInterval)
	defer ticker.Stop()

	for {
		attachment, err := c.observeAttachment(ctx, ecsClient, volumeID, serverID)
		if err == nil && settled(attachment) {
			return attachment, nil
		}
		if err != nil {
			kind := classifyErrorKind(err)
			if !errors.Is(kind, ErrUnavailable) && !errors.Is(kind, ErrOperationFailed) {
				return nil, err
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
