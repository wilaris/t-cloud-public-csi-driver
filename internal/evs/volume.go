package evs

import (
	"context"
	"fmt"
	"strings"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack"
	blockstoragev3 "github.com/opentelekomcloud/gophertelekomcloud/openstack/blockstorage/v3/volumes"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/evs/v2/cloudvolumes"
	v3volumes "github.com/opentelekomcloud/gophertelekomcloud/openstack/evs/v3/volumes"
)

// Volume represents a domain-level representation of an EVS volume.
type Volume struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	Size             int               `json:"size"`
	AvailabilityZone string            `json:"availability_zone"`
	VolumeType       string            `json:"volume_type"`
	Metadata         map[string]string `json:"metadata"`
	Tags             map[string]string `json:"tags"`
	Multiattach      bool              `json:"multiattach"`
}

// CreateVolumeOpts contains parameters for creating an EVS volume.
type CreateVolumeOpts struct {
	AvailabilityZone string            `json:"availability_zone"`
	VolumeType       string            `json:"volume_type"`
	Name             string            `json:"name,omitempty"`
	Description      string            `json:"description,omitempty"`
	Size             int               `json:"size,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
	Multiattach      bool              `json:"multiattach,omitempty"`
}

// ListVolumeOpts contains parameters for listing EVS volumes.
type ListVolumeOpts struct {
	Name             string `json:"name,omitempty"`
	Status           string `json:"status,omitempty"`
	AvailabilityZone string `json:"availability_zone,omitempty"`
	ID               string `json:"id,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	Offset           int    `json:"offset,omitempty"`
}

// Client manages EVS volume lifecycle operations.
type Client struct {
	cfg       Config
	provider  *golangsdk.ProviderClient
	v3Client  *golangsdk.ServiceClient
	v2Client  *golangsdk.ServiceClient
	ecsClient *golangsdk.ServiceClient
}

// NewClient constructs a Client from a validated Config.
func NewClient(cfg Config) (*Client, error) {
	provider, err := NewProviderClient(cfg)
	if err != nil {
		return nil, err
	}
	return NewClientFromProvider(provider, cfg)
}

// NewClientFromProvider constructs a Client using an existing ProviderClient and Config.
func NewClientFromProvider(provider *golangsdk.ProviderClient, cfg Config) (*Client, error) {
	v3Client, err := openstack.NewBlockStorageV3(provider, golangsdk.EndpointOpts{
		Region: cfg.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"failed to create block storage v3 service client: %w",
			sanitizeError(err, cfg),
		)
	}

	v2Client, err := openstack.NewBlockStorageV2(provider, golangsdk.EndpointOpts{
		Region: cfg.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"failed to create block storage v2 service client: %w",
			sanitizeError(err, cfg),
		)
	}

	ecsClient, err := openstack.NewComputeV1(provider, golangsdk.EndpointOpts{
		Region: cfg.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"failed to create ECS v1 service client: %w",
			sanitizeError(err, cfg),
		)
	}

	return &Client{
		cfg:       cfg,
		provider:  provider,
		v3Client:  v3Client,
		v2Client:  v2Client,
		ecsClient: ecsClient,
	}, nil
}

// NewClientWithServiceClients constructs a Client with explicit service clients (for testing).
func NewClientWithServiceClients(
	v3Client, v2Client, ecsClient *golangsdk.ServiceClient,
	cfg Config,
) *Client {
	return &Client{
		cfg:       cfg,
		v3Client:  v3Client,
		v2Client:  v2Client,
		ecsClient: ecsClient,
	}
}

// CreateVolume creates a new EVS volume and polls job status until complete or context is cancelled.
func (c *Client) CreateVolume(ctx context.Context, opts CreateVolumeOpts) (*Volume, error) {
	if ctx.Err() != nil {
		return nil, c.classifyError("create volume", ctx.Err())
	}
	if strings.TrimSpace(opts.AvailabilityZone) == "" {
		return nil, fmt.Errorf(
			"create volume: availability_zone is required: %w",
			ErrInvalidArgument,
		)
	}
	if strings.TrimSpace(opts.VolumeType) == "" {
		return nil, fmt.Errorf("create volume: volume_type is required: %w", ErrInvalidArgument)
	}
	if opts.Size <= 0 {
		return nil, fmt.Errorf(
			"create volume: volume size must be greater than 0: %w",
			ErrInvalidArgument,
		)
	}

	createOpts := v3volumes.CreateOpts{
		AvailabilityZone: opts.AvailabilityZone,
		VolumeType:       opts.VolumeType,
		Name:             opts.Name,
		Description:      opts.Description,
		Size:             opts.Size,
		Multiattach:      opts.Multiattach,
		Metadata:         opts.Metadata,
		Tags:             opts.Tags,
	}

	res := v3volumes.Create(c.v3Client, createOpts)
	if res.Err != nil {
		return nil, c.classifyError("create volume", res.Err)
	}

	jobResp, err := res.ExtractJobResponse()
	if err != nil {
		return nil, c.classifyError("create volume", err)
	}
	if jobResp.JobID == "" {
		return nil, fmt.Errorf("create volume: response missing job_id: %w", ErrOperationFailed)
	}

	if err := c.waitForJobSuccess(ctx, jobResp.JobID); err != nil {
		return nil, c.classifyError("create volume: wait for job", err)
	}

	volIDInterface, err := v3volumes.GetJobEntity(c.v3Client, jobResp.JobID, "volume_id")
	if err != nil {
		return nil, c.classifyError("create volume: get job entity", err)
	}
	volID, ok := volIDInterface.(string)
	if !ok || volID == "" {
		return nil, fmt.Errorf(
			"create volume: job entity did not return a valid volume_id: %w",
			ErrOperationFailed,
		)
	}

	return c.GetVolume(ctx, volID)
}

// GetVolume retrieves an EVS volume by ID.
func (c *Client) GetVolume(ctx context.Context, id string) (*Volume, error) {
	if ctx.Err() != nil {
		return nil, c.classifyError("get volume", ctx.Err())
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("get volume: volume id cannot be empty: %w", ErrInvalidArgument)
	}

	res := v3volumes.Get(c.v3Client, id)
	v3Vol, err := res.Extract()
	if err != nil {
		return nil, c.classifyError(fmt.Sprintf("get volume %s", id), err)
	}

	return mapV3VolumeToDomain(v3Vol), nil
}

// ListVolumes lists EVS volumes matching the given criteria.
func (c *Client) ListVolumes(ctx context.Context, opts ListVolumeOpts) ([]Volume, error) {
	if ctx.Err() != nil {
		return nil, c.classifyError("list volumes", ctx.Err())
	}

	listOpts := cloudvolumes.ListOpts{
		Name:             opts.Name,
		Status:           opts.Status,
		AvailabilityZone: opts.AvailabilityZone,
		ID:               opts.ID,
		Limit:            opts.Limit,
		Offset:           opts.Offset,
	}

	v2Vols, err := cloudvolumes.List(c.v2Client, listOpts)
	if err != nil {
		return nil, c.classifyError("list volumes", err)
	}

	res := make([]Volume, len(v2Vols))
	for i := range v2Vols {
		res[i] = mapV2VolumeToDomain(&v2Vols[i])
	}
	return res, nil
}

// DeleteVolume deletes an EVS volume by ID.
func (c *Client) DeleteVolume(ctx context.Context, id string) error {
	if ctx.Err() != nil {
		return c.classifyError("delete volume", ctx.Err())
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("delete volume: volume id cannot be empty: %w", ErrInvalidArgument)
	}

	res := blockstoragev3.Delete(c.v3Client, id)
	if res.Err != nil {
		return c.classifyError(fmt.Sprintf("delete volume %s", id), res.Err)
	}

	return nil
}

// waitForJobSuccess polls job status until SUCCESS or FAIL, respecting context cancellation.
func (c *Client) waitForJobSuccess(ctx context.Context, jobID string) error {
	jobClient := *c.v3Client
	jobClient.Endpoint = strings.Replace(jobClient.Endpoint, "v3", "v1", 1)
	jobClient.ResourceBase = jobClient.Endpoint

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		job := new(v3volumes.JobStatus)
		_, err := jobClient.Get(jobClient.ServiceURL("jobs", jobID), &job, nil)
		if err != nil {
			return err
		}

		if job.Status == "SUCCESS" {
			return nil
		}
		if job.Status == "FAIL" {
			return fmt.Errorf(
				"EVS job %s failed (code %s): %s",
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

// mapV3VolumeToDomain maps an OpenStack BlockStorage v3 volume struct to a domain Volume.
func mapV3VolumeToDomain(v *v3volumes.Volume) *Volume {
	if v == nil {
		return nil
	}
	metadata := make(map[string]string)
	for k, val := range v.Metadata {
		metadata[k] = val
	}
	tags := make(map[string]string)
	for k, val := range v.Tags {
		tags[k] = val
	}
	return &Volume{
		ID:               v.ID,
		Name:             v.Name,
		Status:           v.Status,
		Size:             v.Size,
		AvailabilityZone: v.AvailabilityZone,
		VolumeType:       v.VolumeType,
		Metadata:         metadata,
		Tags:             tags,
		Multiattach:      v.Multiattach,
	}
}

// mapV2VolumeToDomain maps an OpenStack CloudVolume v2 struct to a domain Volume.
func mapV2VolumeToDomain(v *cloudvolumes.Volume) Volume {
	metadata := make(map[string]string)
	if v.Metadata.SystemCmkID != "" {
		metadata["__system__cmkid"] = v.Metadata.SystemCmkID
	}
	if v.Metadata.SystemEncrypted != "" {
		metadata["__system__encrypted"] = v.Metadata.SystemEncrypted
	}
	if v.Metadata.FullClone != "" {
		metadata["full_clone"] = v.Metadata.FullClone
	}
	if v.Metadata.HwPassthrough != "" {
		metadata["hw:passthrough"] = v.Metadata.HwPassthrough
	}
	if v.Metadata.OrderID != "" {
		metadata["orderID"] = v.Metadata.OrderID
	}
	if v.Metadata.ResourceType != "" {
		metadata["resourceType"] = v.Metadata.ResourceType
	}
	if v.Metadata.ResourceSpecCode != "" {
		metadata["resourceSpecCode"] = v.Metadata.ResourceSpecCode
	}
	if v.Metadata.ReadOnly != "" {
		metadata["readonly"] = v.Metadata.ReadOnly
	}
	if v.Metadata.AttachedMode != "" {
		metadata["attached_mode"] = v.Metadata.AttachedMode
	}

	tags := make(map[string]string)
	for k, val := range v.Tags {
		tags[k] = val
	}

	return Volume{
		ID:               v.ID,
		Name:             v.Name,
		Status:           v.Status,
		Size:             v.Size,
		AvailabilityZone: v.AvailabilityZone,
		VolumeType:       v.VolumeType,
		Metadata:         metadata,
		Tags:             tags,
		Multiattach:      v.Multiattach,
	}
}
