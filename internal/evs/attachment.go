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
)

const attachmentPollInterval = time.Second

// detachKeepVolume tells disk.Detach to only detach; a nonzero delete flag would delete the
// volume along with the attachment.
const detachKeepVolume = 0

// Attachment describes one observed attachment of an EVS volume to a compute instance.
// DeviceName is required; the node cannot find the device without it.
type Attachment struct {
	VolumeID   string
	ServerID   string
	DeviceName string
}

// AttachVolume attaches volumeID to serverID and returns the attachment (incl. already attached).
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
		// Attached but device name not ready yet; wait for it.
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
		kind := classifyErrorKind(err)
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
			// Prefer the context error when reconciliation is canceled or times out.
			if ctx.Err() != nil {
				return nil, c.classifyError(operation, ctx.Err())
			}
		}
		return nil, c.classifyError(operation, err)
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

	if err := c.waitForAttachmentJob(ctx, job.JobID); err != nil {
		return nil, c.classifyError(operation, err)
	}
	attached, err := c.waitForAttachmentState(ctx, volumeID, serverID, attachedWithDevice)
	if err != nil {
		return nil, c.classifyError(operation, err)
	}

	return attached, nil
}

// DetachVolume detaches an EVS volume from the compute instance identified by serverID.
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
		kind := classifyErrorKind(err)
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
			// Prefer the context error when reconciliation is canceled or times out.
			if ctx.Err() != nil {
				return c.classifyError(operation, ctx.Err())
			}
		}
		return c.classifyError(operation, err)
	}
	if job == nil || strings.TrimSpace(job.JobID) == "" {
		if _, err := c.waitForAttachmentState(ctx, volumeID, serverID, detached); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return c.classifyError(operation, ctx.Err())
		}
		return fmt.Errorf("%s: %w", operation, ErrOperationFailed)
	}

	if err := c.waitForAttachmentJob(ctx, job.JobID); err != nil {
		return c.classifyError(operation, err)
	}
	if _, err := c.waitForAttachmentState(ctx, volumeID, serverID, detached); err != nil {
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

// observeAttachment returns the matching ECS attachment or nil if absent.
// The client is reused across polling attempts.
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
		if attachment.VolumeID == volumeID && attachment.ServerID == serverID {
			return &Attachment{
				VolumeID:   attachment.VolumeID,
				ServerID:   attachment.ServerID,
				DeviceName: strings.TrimSpace(attachment.Device),
			}, nil
		}
	}
	return nil, nil
}

// waitForAttachmentJob polls directly because the SDK helper does not support context cancellation
// and starts a goroutine for each poll.
func (c *Client) waitForAttachmentJob(
	ctx context.Context,
	jobID string,
) error {
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
		//nolint:bodyclose // the SDK closes the response body when it decodes into the response value
		if _, err := jobClient.Get(jobClient.ServiceURL("jobs", jobID), &job, nil); err != nil {
			return err
		}

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
