package evs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opentelekomcloud/gophertelekomcloud/openstack/ecs/v1/cloudservers"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/ecs/v1/disk"
)

const (
	attachmentTimeout      = 10 * time.Minute
	attachmentPollInterval = time.Second
)

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
	if err := c.validateAttachmentInput(volumeID, serverID); err != nil {
		return nil, fmt.Errorf("attach volume %s to server %s: %w", volumeID, serverID, err)
	}

	existing, err := c.observeAttachment(ctx, volumeID, serverID)
	if err != nil {
		return nil, c.classifyError(
			fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
			err,
		)
	}
	if attachedWithDevice(existing) {
		return existing, nil
	}
	if existing != nil {
		// Attached but device name not ready yet; wait for it.
		attached, err := c.waitForAttachmentState(ctx, volumeID, serverID, attachedWithDevice)
		if err != nil {
			return nil, c.classifyError(
				fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
				err,
			)
		}
		return attached, nil
	}

	job, err := disk.Attach(c.ecsClient, disk.CreateOpts{
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
		}
		return nil, c.classifyError(
			fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
			err,
		)
	}
	if job == nil || strings.TrimSpace(job.JobID) == "" {
		attached, err := c.waitForAttachmentState(ctx, volumeID, serverID, attachedWithDevice)
		if err == nil {
			return attached, nil
		}
		return nil, fmt.Errorf(
			"attach volume %s to server %s: %w",
			volumeID,
			serverID,
			ErrOperationFailed,
		)
	}

	if err := c.waitForAttachmentJob(ctx, job.JobID); err != nil {
		return nil, c.classifyError(
			fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
			err,
		)
	}
	attached, err := c.waitForAttachmentState(ctx, volumeID, serverID, attachedWithDevice)
	if err != nil {
		return nil, c.classifyError(
			fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
			err,
		)
	}

	return attached, nil
}

// DetachVolume detaches an EVS volume from the compute instance identified by serverID.
func (c *Client) DetachVolume(ctx context.Context, volumeID, serverID string) error {
	if err := c.validateAttachmentInput(volumeID, serverID); err != nil {
		return fmt.Errorf("detach volume %s from server %s: %w", volumeID, serverID, err)
	}

	existing, err := c.observeAttachment(ctx, volumeID, serverID)
	if err != nil {
		return c.classifyError(
			fmt.Sprintf("detach volume %s from server %s", volumeID, serverID),
			err,
		)
	}
	if detached(existing) {
		return nil
	}

	job, err := disk.Detach(c.ecsClient, serverID, volumeID, 0)
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
		}
		return c.classifyError(
			fmt.Sprintf("detach volume %s from server %s", volumeID, serverID),
			err,
		)
	}
	if job == nil || strings.TrimSpace(job.JobID) == "" {
		if _, err := c.waitForAttachmentState(ctx, volumeID, serverID, detached); err == nil {
			return nil
		}
		return fmt.Errorf(
			"detach volume %s from server %s: %w",
			volumeID,
			serverID,
			ErrOperationFailed,
		)
	}

	if err := c.waitForAttachmentJob(ctx, job.JobID); err != nil {
		return c.classifyError(
			fmt.Sprintf("detach volume %s from server %s", volumeID, serverID),
			err,
		)
	}
	if _, err := c.waitForAttachmentState(ctx, volumeID, serverID, detached); err != nil {
		return c.classifyError(
			fmt.Sprintf("detach volume %s from server %s", volumeID, serverID),
			err,
		)
	}

	return nil
}

// attachedWithDevice is true when DeviceName is set.
func attachedWithDevice(attachment *Attachment) bool {
	return attachment != nil && attachment.DeviceName != ""
}

// detached reports whether an observation shows the volume is no longer attached to the server.
func detached(attachment *Attachment) bool {
	return attachment == nil
}

// validateAttachmentInput verifies volumeID and serverID parameters are non-empty strings.
func (c *Client) validateAttachmentInput(volumeID, serverID string) error {
	if strings.TrimSpace(volumeID) == "" || strings.TrimSpace(serverID) == "" {
		return ErrInvalidArgument
	}
	return nil
}

// observeAttachment reads current attachment from ECS (nil if not attached).
func (c *Client) observeAttachment(
	ctx context.Context,
	volumeID, serverID string,
) (*Attachment, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	attachments, err := disk.GetAttachments(c.ecsClient, serverID)
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

// waitForAttachmentJob polls an asynchronous attachment ECS job until completion using cloudservers.WaitForJobSuccess.
func (c *Client) waitForAttachmentJob(
	ctx context.Context,
	jobID string,
) error {
	var seconds int
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		seconds = int((remaining + time.Second - 1) / time.Second)
	} else {
		seconds = int(attachmentTimeout / time.Second)
	}

	if err := cloudservers.WaitForJobSuccess(c.ecsClient, seconds, jobID); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// waitForAttachmentState polls attachment status until settled accepts the observation, returning
// that observation, or until the context expires.
func (c *Client) waitForAttachmentState(
	ctx context.Context,
	volumeID, serverID string,
	settled func(*Attachment) bool,
) (*Attachment, error) {
	ticker := time.NewTicker(attachmentPollInterval)
	defer ticker.Stop()

	for {
		attachment, err := c.observeAttachment(ctx, volumeID, serverID)
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
