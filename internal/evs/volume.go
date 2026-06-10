package evs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack"
	blockstoragev3 "github.com/opentelekomcloud/gophertelekomcloud/openstack/blockstorage/v3/volumes"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/evs/v2/cloudvolumes"
	v3volumes "github.com/opentelekomcloud/gophertelekomcloud/openstack/evs/v3/volumes"
)

const (
	// OwnershipTagKey is the EVS tag key that marks a volume as created by this driver.
	OwnershipTagKey = "managed-by"
	// OwnershipTagValue is the fixed plugin identity recorded in the ownership tag.
	OwnershipTagValue = "evs.csi.t-cloud.wilaris.dev"

	volumeAbsencePollInterval = time.Second
	volumeAbsenceMaxAttempts  = 60

	// volumeListPageSize is the page size requested for a paged listing, so discovery does not
	// depend on a server-chosen default.
	volumeListPageSize = 100
	// volumeListMaxPages bounds a paged listing so that no single discovery call can walk an
	// unbounded number of pages.
	volumeListMaxPages = 50

	volumeStatusAvailable     = "available"
	volumeStatusInUse         = "in-use"
	volumeStatusDeleting      = "deleting"
	volumeStatusErrorDeleting = "error_deleting"
)

// Volume describes an EVS volume.
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

// CreateVolumeOpts contains volume parameters. CreateVolume applies the ownership tag.
type CreateVolumeOpts struct {
	AvailabilityZone string `json:"availability_zone"`
	VolumeType       string `json:"volume_type"`
	Name             string `json:"name,omitempty"`
	Description      string `json:"description,omitempty"`
	Size             int    `json:"size,omitempty"`
	Multiattach      bool   `json:"multiattach,omitempty"`
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

// DiscoverVolumeOpts describes the requested volume for same-name adoption.
// MaxSizeGiB is an inclusive upper bound; zero means the caller declares no upper bound.
type DiscoverVolumeOpts struct {
	Name             string `json:"name"`
	AvailabilityZone string `json:"availability_zone"`
	VolumeType       string `json:"volume_type"`
	MinSizeGiB       int    `json:"min_size_gib"`
	MaxSizeGiB       int    `json:"max_size_gib,omitempty"`
}

// Client manages EVS volume lifecycle operations.
type Client struct {
	cfg       Config
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

// CreateVolume creates an EVS volume and polls its job until completion or context cancellation.
func (c *Client) CreateVolume(ctx context.Context, opts CreateVolumeOpts) (*Volume, error) {
	ctx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()

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
		Tags:             map[string]string{OwnershipTagKey: OwnershipTagValue},
	}

	res := v3volumes.Create(c.v3(ctx), createOpts)
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

	// If this lookup fails after the job succeeded, the created volume is orphaned: the
	// caller receives an error without the volume ID and cannot reference or clean it up.
	volIDInterface, err := v3volumes.GetJobEntity(c.v3(ctx), jobResp.JobID, "volume_id")
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
	ctx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()

	if ctx.Err() != nil {
		return nil, c.classifyError("get volume", ctx.Err())
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("get volume: volume id cannot be empty: %w", ErrInvalidArgument)
	}

	res := v3volumes.Get(c.v3(ctx), id)
	v3Vol, err := res.Extract()
	if err != nil {
		return nil, c.classifyError(fmt.Sprintf("get volume %s", id), err)
	}

	return mapV3VolumeToDomain(v3Vol), nil
}

// ListVolumes lists EVS volumes matching the given criteria. A caller that supplies its own Limit or
// Offset receives exactly that page. A caller that supplies neither receives every page within the
// listing bound.
func (c *Client) ListVolumes(ctx context.Context, opts ListVolumeOpts) ([]Volume, error) {
	ctx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()

	if opts.Limit > 0 || opts.Offset > 0 {
		return c.listVolumePage(ctx, opts)
	}

	// The bound counts non-empty pages; one extra fetch is allowed so a listing that ends
	// exactly on the bound is confirmed by the empty page that terminates it.
	var res []Volume
	for pages := 0; pages <= volumeListMaxPages; pages++ {
		pageOpts := opts
		pageOpts.Limit = volumeListPageSize
		pageOpts.Offset = len(res)

		page, err := c.listVolumePage(ctx, pageOpts)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return res, nil
		}
		res = append(res, page...)
	}

	return nil, fmt.Errorf(
		"list volumes: more pages than the listing bound allows: %w",
		ErrOperationFailed,
	)
}

// listVolumePage lists one page of EVS volumes.
func (c *Client) listVolumePage(ctx context.Context, opts ListVolumeOpts) ([]Volume, error) {
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

	v2Vols, err := cloudvolumes.List(c.v2(ctx), listOpts)
	if err != nil {
		return nil, c.classifyError("list volumes", err)
	}

	res := make([]Volume, len(v2Vols))
	for i := range v2Vols {
		res[i] = mapV2VolumeToDomain(&v2Vols[i])
	}
	return res, nil
}

// DiscoverVolume returns the volume named by opts when this driver owns it and it satisfies the
// requested availability zone, volume type and size bounds. It returns ErrNotFound when no volume
// carries that name. It returns ErrConflict when a same-name volume exists that this driver does
// not own or cannot adopt. No candidate is modified.
func (c *Client) DiscoverVolume(ctx context.Context, opts DiscoverVolumeOpts) (*Volume, error) {
	ctx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()

	if strings.TrimSpace(opts.Name) == "" {
		return nil, fmt.Errorf("discover volume: name cannot be empty: %w", ErrInvalidArgument)
	}

	candidates, err := c.ListVolumes(ctx, ListVolumeOpts{Name: opts.Name})
	if err != nil {
		return nil, err
	}

	// Verify exact names because server-side filtering may be fuzzy or ignored.
	named := make([]Volume, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Name == opts.Name {
			named = append(named, candidate)
		}
	}
	if len(named) == 0 {
		return nil, fmt.Errorf("discover volume: no volume with that name: %w", ErrNotFound)
	}

	// Sort by ID for deterministic adoption when names are duplicated.
	slices.SortFunc(named, func(a, b Volume) int {
		return strings.Compare(a.ID, b.ID)
	})

	for i := range named {
		if isAdoptable(&named[i], opts) {
			return &named[i], nil
		}
	}

	return nil, fmt.Errorf(
		"discover volume: an existing volume with that name is not owned by this driver"+
			" or is incompatible with the request: %w",
		ErrConflict,
	)
}

// isAdoptable reports whether an existing volume carries the ownership marker and satisfies the
// requested availability zone, volume type, size bounds and an attachable status.
func isAdoptable(vol *Volume, opts DiscoverVolumeOpts) bool {
	if vol.Tags[OwnershipTagKey] != OwnershipTagValue {
		return false
	}
	if vol.Status != volumeStatusAvailable && vol.Status != volumeStatusInUse {
		return false
	}
	if vol.AvailabilityZone != opts.AvailabilityZone {
		return false
	}
	if vol.VolumeType != opts.VolumeType {
		return false
	}
	if vol.Size < opts.MinSizeGiB {
		return false
	}
	if opts.MaxSizeGiB > 0 && vol.Size > opts.MaxSizeGiB {
		return false
	}
	return true
}

// DeleteVolume deletes a driver-owned volume and waits until it is absent. It returns ErrNotOwned
// before issuing a destructive call when the ownership marker is missing and ErrNotFound when the
// volume is absent. Existing deletions are polled without another request. A cloud-reported deletion
// failure is terminal.
func (c *Client) DeleteVolume(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()

	if ctx.Err() != nil {
		return c.classifyError("delete volume", ctx.Err())
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("delete volume: volume id cannot be empty: %w", ErrInvalidArgument)
	}

	vol, err := c.GetVolume(ctx, id)
	if err != nil {
		return err
	}
	if vol.Tags[OwnershipTagKey] != OwnershipTagValue {
		return fmt.Errorf(
			"delete volume: volume carries no ownership marker: %w",
			ErrNotOwned,
		)
	}

	switch vol.Status {
	case volumeStatusErrorDeleting:
		return fmt.Errorf(
			"delete volume: cloud reports a failed deletion for this volume: %w",
			ErrOperationFailed,
		)
	case volumeStatusDeleting:
		// A deletion is already in progress.
	default:
		res := blockstoragev3.Delete(c.v3(ctx), id)
		if res.Err != nil {
			return c.classifyError(fmt.Sprintf("delete volume %s", id), res.Err)
		}
	}

	return c.waitForVolumeAbsence(ctx, id)
}

// waitForVolumeAbsence retries transient poll failures until the volume is absent or the attempt limit is reached.
func (c *Client) waitForVolumeAbsence(ctx context.Context, id string) error {
	ticker := time.NewTicker(volumeAbsencePollInterval)
	defer ticker.Stop()

	for attempt := range volumeAbsenceMaxAttempts {
		_, err := c.GetVolume(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			kind := classifyErrorKind(err)
			if !errors.Is(kind, ErrUnavailable) && !errors.Is(kind, ErrOperationFailed) {
				return err
			}
		}
		if attempt == volumeAbsenceMaxAttempts-1 {
			break
		}

		select {
		case <-ctx.Done():
			return c.classifyError("delete volume: wait for absence", ctx.Err())
		case <-ticker.C:
		}
	}

	return fmt.Errorf(
		"delete volume: absence was not observable within the deletion bound: %w",
		ErrOperationFailed,
	)
}

// waitForJobSuccess polls job status until SUCCESS or FAIL, respecting context cancellation.
func (c *Client) waitForJobSuccess(ctx context.Context, jobID string) error {
	jobClient := *c.v3(ctx)
	jobEndpoint, err := jobStatusEndpoint(jobClient.Endpoint)
	if err != nil {
		return err
	}
	jobClient.Endpoint = jobEndpoint
	jobClient.ResourceBase = jobEndpoint

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		job := new(v3volumes.JobStatus)
		//nolint:bodyclose // the SDK closes the response body when it decodes into the response value
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

// jobStatusEndpoint rewrites a block storage v3 endpoint to the v1 endpoint that serves job
// status. Only the leading "/v3/" path segment is replaced; a "v3" anywhere else in the URL
// (host, port, prefix) must be left untouched.
func jobStatusEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("wait for job: parse endpoint: %w: %s", ErrOperationFailed, err)
	}
	rest, ok := strings.CutPrefix(u.Path, "/v3/")
	if !ok {
		return "", fmt.Errorf(
			"wait for job: endpoint path %q does not start with /v3/: %w",
			u.Path,
			ErrOperationFailed,
		)
	}
	u.Path = "/v1/" + rest
	return u.String(), nil
}

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
