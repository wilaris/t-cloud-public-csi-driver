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

// AttachVolume attaches an EVS volume to the compute instance identified by serverID.
func (c *Client) AttachVolume(ctx context.Context, volumeID, serverID string) error {
	if err := c.validateAttachmentInput(volumeID, serverID); err != nil {
		return fmt.Errorf("attach volume %s to server %s: %w", volumeID, serverID, err)
	}

	attached, err := c.attachmentExists(ctx, volumeID, serverID)
	if err != nil {
		return c.classifyError(
			fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
			err,
		)
	}
	if attached {
		return nil
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
			observeErr := c.waitForAttachmentState(
				ctx,
				volumeID,
				serverID,
				true,
			)
			if observeErr == nil {
				return nil
			}
		}
		return c.classifyError(
			fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
			err,
		)
	}
	if job == nil || strings.TrimSpace(job.JobID) == "" {
		if err := c.waitForAttachmentState(ctx, volumeID, serverID, true); err == nil {
			return nil
		}
		return fmt.Errorf(
			"attach volume %s to server %s: %w",
			volumeID,
			serverID,
			ErrOperationFailed,
		)
	}

	if err := c.waitForAttachmentJob(ctx, job.JobID); err != nil {
		return c.classifyError(
			fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
			err,
		)
	}
	if err := c.waitForAttachmentState(ctx, volumeID, serverID, true); err != nil {
		return c.classifyError(
			fmt.Sprintf("attach volume %s to server %s", volumeID, serverID),
			err,
		)
	}

	return nil
}

// DetachVolume detaches an EVS volume from the compute instance identified by serverID.
func (c *Client) DetachVolume(ctx context.Context, volumeID, serverID string) error {
	if err := c.validateAttachmentInput(volumeID, serverID); err != nil {
		return fmt.Errorf("detach volume %s from server %s: %w", volumeID, serverID, err)
	}

	attached, err := c.attachmentExists(ctx, volumeID, serverID)
	if err != nil {
		return c.classifyError(
			fmt.Sprintf("detach volume %s from server %s", volumeID, serverID),
			err,
		)
	}
	if !attached {
		return nil
	}

	job, err := disk.Detach(c.ecsClient, serverID, volumeID, 0)
	if err != nil {
		kind := classifyErrorKind(err)
		if errors.Is(kind, ErrUnavailable) || errors.Is(kind, ErrOperationFailed) {
			observeErr := c.waitForAttachmentState(
				ctx,
				volumeID,
				serverID,
				false,
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
		if err := c.waitForAttachmentState(ctx, volumeID, serverID, false); err == nil {
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
	if err := c.waitForAttachmentState(ctx, volumeID, serverID, false); err != nil {
		return c.classifyError(
			fmt.Sprintf("detach volume %s from server %s", volumeID, serverID),
			err,
		)
	}

	return nil
}

// validateAttachmentInput verifies volumeID and serverID parameters are non-empty strings.
func (c *Client) validateAttachmentInput(volumeID, serverID string) error {
	if strings.TrimSpace(volumeID) == "" || strings.TrimSpace(serverID) == "" {
		return ErrInvalidArgument
	}
	return nil
}

// attachmentExists checks whether the specified volume is attached to serverID.
func (c *Client) attachmentExists(
	ctx context.Context,
	volumeID, serverID string,
) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	attachments, err := disk.GetAttachments(c.ecsClient, serverID)
	if err != nil {
		return false, err
	}
	if attachments == nil {
		return false, nil
	}

	for _, attachment := range attachments.VolumeAttachments {
		if attachment.VolumeID == volumeID && attachment.ServerID == serverID {
			return true, nil
		}
	}
	return false, nil
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

// waitForAttachmentState polls attachment status until the volume matches wantAttached state or context expires.
func (c *Client) waitForAttachmentState(
	ctx context.Context,
	volumeID, serverID string,
	wantAttached bool,
) error {
	ticker := time.NewTicker(attachmentPollInterval)
	defer ticker.Stop()

	for {
		attached, err := c.attachmentExists(ctx, volumeID, serverID)
		if err == nil && attached == wantAttached {
			return nil
		}

		if err != nil {
			kind := classifyErrorKind(err)
			if !errors.Is(kind, ErrUnavailable) && !errors.Is(kind, ErrOperationFailed) {
				return err
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
