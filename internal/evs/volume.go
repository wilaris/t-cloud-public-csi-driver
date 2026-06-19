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
	// OwnershipTagKey is the tag key written on every volume this driver creates.
	OwnershipTagKey = "managed-by"
	// OwnershipTagValue is the plugin identity stored under OwnershipTagKey. Hyphens replace
	// dots because EVS tag values accept only letters, digits, underscores, hyphens, and at signs.
	OwnershipTagValue = "evs-csi-t-cloud-wilaris-dev"

	// DiscoveryPageSize is the page length used when a listing walks the project. The
	// conformance asset relies on it to place an adoptable volume past the first page.
	DiscoveryPageSize = 100
	// volumeListMaxPages is the maximum number of non-empty pages one listing may fetch.
	volumeListMaxPages = 50

	volumeStatusAvailable     = "available"
	volumeStatusAttaching     = "attaching"
	volumeStatusInUse         = "in-use"
	volumeStatusDetaching     = "detaching"
	volumeStatusDeleting      = "deleting"
	volumeStatusErrorDeleting = "error_deleting"
)

// Absence polling stays in variables so a package test can shrink the schedule. Callers
// outside this package do not read these values.
var (
	volumeAbsencePollInterval = time.Second
	volumeAbsenceMaxAttempts  = 60
)

// Volume is the driver-facing record of one EVS volume.
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

// CreateVolumeOpts is the create request. The client stamps the ownership tag itself.
type CreateVolumeOpts struct {
	AvailabilityZone string `json:"availability_zone"`
	VolumeType       string `json:"volume_type"`
	Name             string `json:"name,omitempty"`
	Description      string `json:"description,omitempty"`
	Size             int    `json:"size,omitempty"`
	Multiattach      bool   `json:"multiattach,omitempty"`
}

// ListVolumeOpts selects which EVS volumes a listing returns.
type ListVolumeOpts struct {
	Name             string `json:"name,omitempty"`
	Status           string `json:"status,omitempty"`
	AvailabilityZone string `json:"availability_zone,omitempty"`
	ID               string `json:"id,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	Offset           int    `json:"offset,omitempty"`
}

// DiscoverVolumeOpts is the adoption query for a volume of this name.
// MaxSizeGiB is inclusive. Zero leaves the upper bound unconstrained.
type DiscoverVolumeOpts struct {
	Name             string `json:"name"`
	AvailabilityZone string `json:"availability_zone"`
	VolumeType       string `json:"volume_type"`
	MinSizeGiB       int    `json:"min_size_gib"`
	MaxSizeGiB       int    `json:"max_size_gib,omitempty"`
}

// Client is the EVS volume and attachment client. Construct with NewClient or NewClientFromProvider.
type Client struct {
	cfg       Config
	v3Client  *golangsdk.ServiceClient
	v2Client  *golangsdk.ServiceClient
	ecsClient *golangsdk.ServiceClient
}

// NewClient authenticates against the cloud and returns a Client. ctx bounds only that exchange.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	provider, err := NewProviderClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return NewClientFromProvider(provider, cfg)
}

// NewClientFromProvider builds service clients from an already authenticated provider.
func NewClientFromProvider(provider *golangsdk.ProviderClient, cfg Config) (*Client, error) {
	v3Client, err := openstack.NewBlockStorageV3(provider, golangsdk.EndpointOpts{
		Region: cfg.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"create block storage v3 service client: %w",
			sanitizeError(err, cfg),
		)
	}

	v2Client, err := openstack.NewBlockStorageV2(provider, golangsdk.EndpointOpts{
		Region: cfg.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"create block storage v2 service client: %w",
			sanitizeError(err, cfg),
		)
	}

	ecsClient, err := openstack.NewComputeV1(provider, golangsdk.EndpointOpts{
		Region: cfg.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"create ECS v1 service client: %w",
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

// NewClientWithServiceClients returns a Client that uses the supplied service clients.
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

// CreateVolume provisions an EVS volume and waits for its job to finish or for ctx to end.
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

	volID, err := c.waitForJobSuccess(ctx, jobResp.JobID)
	if err != nil {
		return nil, c.classifyError("create volume: wait for job", err)
	}

	return c.GetVolume(ctx, volID)
}

// GetVolume returns the EVS volume identified by id.
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

// ListVolumes returns volumes that match opts. A positive Limit or Offset fetches one page;
// otherwise the client walks pages until the list ends or volumeListMaxPages is exceeded.
func (c *Client) ListVolumes(ctx context.Context, opts ListVolumeOpts) ([]Volume, error) {
	ctx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()

	if opts.Limit > 0 || opts.Offset > 0 {
		return c.listVolumePage(ctx, opts)
	}

	// Non-empty pages consume the bound. One more fetch is permitted so a list that lands
	// exactly on the bound can still terminate on the following empty page.
	var res []Volume
	for pages := 0; pages <= volumeListMaxPages; pages++ {
		pageOpts := opts
		pageOpts.Limit = DiscoveryPageSize
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

// listVolumePage fetches a single page from the v2 list API.
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

// DiscoverVolume finds a driver-owned volume that can be adopted for opts.
// It returns ErrNotFound when no volume has that name, and ErrConflict when a
// same-name volume exists but cannot be adopted.
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

	// The service filter can be approximate, so keep only exact name matches.
	named := make([]Volume, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Name == opts.Name {
			named = append(named, candidate)
		}
	}
	if len(named) == 0 {
		return nil, fmt.Errorf("discover volume: no volume with that name: %w", ErrNotFound)
	}

	// Order by ID so two same-name volumes always yield the same choice.
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

// isAdoptable is true when vol carries this driver's marker and matches opts.
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

// DeleteVolume removes a driver-owned volume and waits until GetVolume reports it gone.
// A missing ownership marker returns ErrNotOwned without a DELETE. An already-absent volume
// returns ErrNotFound. A volume already deleting is only observed. A cloud-reported
// error_deleting status is terminal.
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
		// Do not issue another DELETE while the cloud is already deleting.
	default:
		res := blockstoragev3.Delete(c.v3(ctx), id)
		if res.Err != nil {
			return c.classifyError(fmt.Sprintf("delete volume %s", id), res.Err)
		}
	}

	return c.waitForVolumeAbsence(ctx, id)
}

// waitForVolumeAbsence polls GetVolume until the volume is gone, the attempt budget ends,
// or a non-transient error is observed.
func (c *Client) waitForVolumeAbsence(ctx context.Context, id string) error {
	ticker := time.NewTicker(volumeAbsencePollInterval)
	defer ticker.Stop()

	var (
		observedPresent bool
		lastTransient   error
	)
	for attempt := range volumeAbsenceMaxAttempts {
		_, err := c.GetVolume(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err == nil {
			observedPresent = true
		} else {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			kind := classifyErrorKind(err)
			if !errors.Is(kind, ErrUnavailable) && !errors.Is(kind, ErrOperationFailed) {
				return err
			}
			lastTransient = err
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

	if observedPresent {
		return fmt.Errorf(
			"delete volume: volume still present after polling: %w",
			ErrOperationFailed,
		)
	}
	// No poll saw the volume. Keep the last transient kind so a brownout stays retryable.
	return fmt.Errorf(
		"delete volume: absence polling exhausted without observing the volume: %w",
		lastTransient,
	)
}

// waitForJobSuccess waits until the EVS job reaches SUCCESS or FAIL and returns the new volume ID.
func (c *Client) waitForJobSuccess(ctx context.Context, jobID string) (string, error) {
	jobClient := *c.v3(ctx)
	jobEndpoint, err := jobStatusEndpoint(jobClient.Endpoint)
	if err != nil {
		return "", err
	}
	jobClient.Endpoint = jobEndpoint
	jobClient.ResourceBase = jobEndpoint

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		var job v3volumes.JobStatus
		//nolint:bodyclose // the SDK closes the body after decoding into job
		_, err = jobClient.Get(jobClient.ServiceURL("jobs", jobID), &job, nil)
		if err == nil {
			if job.Status == "SUCCESS" {
				volID := strings.TrimSpace(job.Entities.VolumeID)
				if volID == "" {
					return "", fmt.Errorf(
						"EVS job %s succeeded without a volume_id: %w",
						jobID,
						ErrOperationFailed,
					)
				}
				return volID, nil
			}
			if job.Status == "FAIL" {
				return "", fmt.Errorf(
					"EVS job %s failed (code %s): %s",
					jobID,
					job.ErrorCode,
					job.FailReason,
				)
			}
		} else if jobPollShouldStop(err) {
			return "", err
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// jobPollShouldStop is true when a job-status GET should not be retried. Unavailable and
// unclassified operation failures keep polling because the GET only observes work already
// accepted.
func jobPollShouldStop(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	kind := classifyErrorKind(err)
	return !errors.Is(kind, ErrUnavailable) && !errors.Is(kind, ErrOperationFailed)
}

// jobStatusEndpoint maps a block-storage v3 endpoint onto the v1 jobs URL. Only a leading
// "/v3/" path prefix is rewritten so a host or port that happens to contain "v3" is unchanged.
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
