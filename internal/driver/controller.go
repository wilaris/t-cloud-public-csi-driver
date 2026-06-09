// Package driver implements Container Storage Interface (CSI) gRPC services.
package driver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"wilaris.dev/t-cloud-public-csi-driver/internal/mount"
)

const (
	// TopologyZoneKey is the standard CSI topology key for availability zones.
	TopologyZoneKey = "topology.kubernetes.io/zone"
	// paramVolumeType is the accepted StorageClass parameter naming the EVS volume type.
	paramVolumeType = "type"
	// paramAvailabilityZone is the accepted StorageClass parameter naming the availability zone.
	paramAvailabilityZone = "availability_zone"
	// coReservedParameterPrefix is the StorageClass parameter namespace owned by the container
	// orchestrator.
	coReservedParameterPrefix = "csi.storage.k8s.io/"
	// minVolumeSizeGiB is the declared EVS minimum data-volume size. It resolves an omitted or
	// smaller requested capacity; it is never a lower-bound check, so a request is refused only
	// when the resolved size exceeds limit_bytes.
	minVolumeSizeGiB = 10
	// bytesInGiB is the number of bytes in one GiB.
	bytesInGiB = 1024 * 1024 * 1024
)

// EVSClient defines the subset of EVS operations required by the CSI Controller service.
type EVSClient interface {
	CreateVolume(ctx context.Context, opts evs.CreateVolumeOpts) (*evs.Volume, error)
	GetVolume(ctx context.Context, id string) (*evs.Volume, error)
	DiscoverVolume(ctx context.Context, opts evs.DiscoverVolumeOpts) (*evs.Volume, error)
	DeleteVolume(ctx context.Context, id string) error
	AttachVolume(ctx context.Context, volumeID, serverID string) error
	DetachVolume(ctx context.Context, volumeID, serverID string) error
}

// ControllerService implements the csi.ControllerServer interface.
type ControllerService struct {
	csi.UnimplementedControllerServer

	evsClient EVSClient
	cfg       *config.Config
}

// NewControllerService constructs a new ControllerService instance.
func NewControllerService(evsClient EVSClient, cfg *config.Config) (*ControllerService, error) {
	if evsClient == nil {
		return nil, fmt.Errorf("EVS client cannot be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	return &ControllerService{
		evsClient: evsClient,
		cfg:       cfg,
	}, nil
}

// ControllerGetCapabilities returns the capabilities supported by the Controller service.
func (s *ControllerService) ControllerGetCapabilities(
	ctx context.Context,
	req *csi.ControllerGetCapabilitiesRequest,
) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
					},
				},
			},
		},
	}, nil
}

// ControllerPublishVolume attaches an EVS volume to the specified compute instance (node_id).
func (s *ControllerService) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest,
) (*csi.ControllerPublishVolumeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id cannot be empty")
	}
	if strings.TrimSpace(req.GetNodeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id cannot be empty")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume_capability cannot be nil")
	}

	if err := validateVolumeCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}

	// EVS attachments have no read-only form and we don't advertise PUBLISH_READONLY,
	// so reject instead of silently attaching read-write.
	if req.GetReadonly() {
		return nil, status.Error(
			codes.InvalidArgument,
			"read-only publication is not supported by this driver",
		)
	}

	err := s.evsClient.AttachVolume(ctx, req.GetVolumeId(), req.GetNodeId())
	if err != nil {
		return nil, toGRPCError(
			fmt.Sprintf("publish volume %q to node %q", req.GetVolumeId(), req.GetNodeId()),
			codes.FailedPrecondition,
			err,
		)
	}

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{},
	}, nil
}

// ControllerUnpublishVolume detaches an EVS volume from the specified compute instance (node_id).
func (s *ControllerService) ControllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest,
) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id cannot be empty")
	}
	if strings.TrimSpace(req.GetNodeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id cannot be empty")
	}

	err := s.evsClient.DetachVolume(ctx, req.GetVolumeId(), req.GetNodeId())
	if err != nil {
		return nil, toGRPCError(
			fmt.Sprintf("unpublish volume %q from node %q", req.GetVolumeId(), req.GetNodeId()),
			codes.Aborted,
			err,
		)
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// CreateVolume provisions a new volume or returns an existing compatible volume (idempotent).
func (s *ControllerService) CreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
) (*csi.CreateVolumeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, status.Error(codes.InvalidArgument, "name cannot be empty")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities cannot be empty")
	}

	for _, vc := range req.GetVolumeCapabilities() {
		if err := validateVolumeCapability(vc); err != nil {
			return nil, err
		}
	}

	if req.GetVolumeContentSource() != nil {
		return nil, status.Error(
			codes.InvalidArgument,
			"provisioning from a volume content source is not supported",
		)
	}

	params := req.GetParameters()
	if err := validateParameters(params); err != nil {
		return nil, err
	}

	volType := strings.TrimSpace(params[paramVolumeType])
	if volType == "" {
		return nil, status.Error(
			codes.InvalidArgument,
			fmt.Sprintf("StorageClass parameter %q is required", paramVolumeType),
		)
	}

	zone := pickAvailabilityZone(req.GetAccessibilityRequirements(), params)
	if zone == "" {
		return nil, status.Error(
			codes.InvalidArgument,
			"availability zone is required in topology requirements or parameters",
		)
	}

	sizeGiB, err := resolveSizeGiB(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}

	existing, err := s.evsClient.DiscoverVolume(ctx, evs.DiscoverVolumeOpts{
		Name:             req.GetName(),
		AvailabilityZone: zone,
		VolumeType:       volType,
		MinSizeGiB:       sizeGiB,
		MaxSizeGiB:       limitSizeGiB(req.GetCapacityRange()),
	})
	if err == nil {
		return formatCreateVolumeResponse(existing), nil
	}
	if !errors.Is(err, evs.ErrNotFound) {
		return nil, toGRPCError("discover existing volume", codes.AlreadyExists, err)
	}

	created, err := s.evsClient.CreateVolume(ctx, evs.CreateVolumeOpts{
		Name:             req.GetName(),
		Size:             sizeGiB,
		AvailabilityZone: zone,
		VolumeType:       volType,
	})
	if err != nil {
		return nil, toGRPCError("create volume", codes.AlreadyExists, err)
	}

	return formatCreateVolumeResponse(created), nil
}

// DeleteVolume idempotently deletes an EVS volume.
func (s *ControllerService) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest,
) (*csi.DeleteVolumeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id cannot be empty")
	}

	err := s.evsClient.DeleteVolume(ctx, req.GetVolumeId())
	if err != nil {
		if errors.Is(err, evs.ErrNotFound) {
			return &csi.DeleteVolumeResponse{}, nil
		}
		// Refusing to delete a volume we don't own is expected, not an internal error — map it explicitly.
		if errors.Is(err, evs.ErrNotOwned) {
			return nil, status.Error(
				codes.FailedPrecondition,
				fmt.Sprintf(
					"delete volume %q: volume carries no ownership marker for this driver",
					req.GetVolumeId(),
				),
			)
		}
		return nil, toGRPCError(
			fmt.Sprintf("delete volume %q", req.GetVolumeId()),
			codes.FailedPrecondition,
			err,
		)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// ValidateVolumeCapabilities checks if the volume supports the requested capabilities.
func (s *ControllerService) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest,
) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id cannot be empty")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities cannot be empty")
	}

	_, err := s.evsClient.GetVolume(ctx, req.GetVolumeId())
	if err != nil {
		if errors.Is(err, evs.ErrNotFound) {
			return nil, status.Error(
				codes.NotFound,
				fmt.Sprintf("volume %q not found", req.GetVolumeId()),
			)
		}
		return nil, toGRPCError(
			fmt.Sprintf("get volume %q", req.GetVolumeId()),
			codes.Aborted,
			err,
		)
	}

	for _, vc := range req.GetVolumeCapabilities() {
		if err := validateVolumeCapability(vc); err != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Confirmed: nil,
				Message:   "unsupported volume capability or access mode",
			}, nil
		}
	}

	// We provision with an empty volume context, so a non-empty one means the volume isn't ours.
	if len(req.GetVolumeContext()) > 0 {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Confirmed: nil,
			Message:   "unsupported volume context",
		}, nil
	}

	if err := validateParameters(req.GetParameters()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Confirmed: nil,
			Message:   "unsupported volume parameter",
		}, nil
	}

	// Echo the validated fields so the CO can detect fields the plugin ignored.
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}

// validateVolumeCapability rejects unsupported access modes and filesystems before any cloud
// or host side effects. SINGLE_NODE_SINGLE_WRITER and SINGLE_NODE_MULTI_WRITER are rejected
// because the CSI spec gates them behind a Node capability we don't advertise.
func validateVolumeCapability(cap *csi.VolumeCapability) error {
	if cap == nil {
		return status.Error(codes.InvalidArgument, "volume capability cannot be nil")
	}
	accessMode := cap.GetAccessMode()
	if accessMode == nil {
		return status.Error(codes.InvalidArgument, "access mode cannot be nil")
	}

	switch accessMode.GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
	default:
		return status.Error(
			codes.InvalidArgument,
			fmt.Sprintf(
				"unsupported access mode %s: only SINGLE_NODE_WRITER and SINGLE_NODE_READER_ONLY are supported",
				accessMode.GetMode(),
			),
		)
	}

	// Raw block access carries no filesystem and is never formatted, so only a mounted capability has
	// a filesystem to check. An omitted name resolves to the default at staging time.
	fsType := strings.TrimSpace(cap.GetMount().GetFsType())
	if fsType != "" && !mount.IsSupportedFilesystemType(fsType) {
		return status.Error(
			codes.InvalidArgument,
			"unsupported filesystem: only ext4 and xfs are supported",
		)
	}

	return nil
}

// validateParameters rejects StorageClass parameter keys the driver does not accept. Keys reserved
// for the container orchestrator are ignored, because the CO owns that namespace.
func validateParameters(params map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(params)) {
		switch {
		case strings.HasPrefix(key, coReservedParameterPrefix):
			continue
		case key == paramVolumeType, key == paramAvailabilityZone:
			continue
		default:
			return status.Error(
				codes.InvalidArgument,
				fmt.Sprintf("unsupported StorageClass parameter %q", key),
			)
		}
	}

	return nil
}

// resolveSizeGiB resolves a CSI capacity range to a whole-GiB EVS volume size. An omitted range or a
// required size below the declared EVS minimum resolves to that minimum. A present required size is
// rounded up to whole GiB and never capped, leaving every upper bound to EVS.
func resolveSizeGiB(capRange *csi.CapacityRange) (int, error) {
	reqBytes := capRange.GetRequiredBytes()
	limitBytes := capRange.GetLimitBytes()

	if reqBytes < 0 || limitBytes < 0 {
		return 0, status.Error(codes.InvalidArgument, "capacity bytes cannot be negative")
	}
	if limitBytes > 0 && reqBytes > limitBytes {
		return 0, status.Error(
			codes.InvalidArgument,
			fmt.Sprintf("required_bytes (%d) exceeds limit_bytes (%d)", reqBytes, limitBytes),
		)
	}

	roundedBytes := reqBytes + bytesInGiB - 1
	if roundedBytes < reqBytes {
		return 0, status.Error(
			codes.OutOfRange,
			"required_bytes exceeds the largest expressible volume size",
		)
	}

	sizeGiB := max(int(roundedBytes/bytesInGiB), minVolumeSizeGiB)

	if limitBytes > 0 && int64(sizeGiB)*bytesInGiB > limitBytes {
		return 0, status.Error(
			codes.OutOfRange,
			fmt.Sprintf("no supported volume size satisfies limit_bytes (%d)", limitBytes),
		)
	}

	return sizeGiB, nil
}

// limitSizeGiB converts a capacity range upper bound to whole GiB, returning zero when the request
// declares no upper bound.
func limitSizeGiB(capRange *csi.CapacityRange) int {
	limitBytes := capRange.GetLimitBytes()
	if limitBytes <= 0 {
		return 0
	}

	return int(limitBytes / bytesInGiB)
}

// pickAvailabilityZone selects an availability zone from topology requirements or parameters.
func pickAvailabilityZone(topReq *csi.TopologyRequirement, params map[string]string) string {
	if topReq != nil {
		for _, top := range topReq.GetPreferred() {
			if zone, ok := top.GetSegments()[TopologyZoneKey]; ok && strings.TrimSpace(zone) != "" {
				return strings.TrimSpace(zone)
			}
		}
		for _, top := range topReq.GetRequisite() {
			if zone, ok := top.GetSegments()[TopologyZoneKey]; ok && strings.TrimSpace(zone) != "" {
				return strings.TrimSpace(zone)
			}
		}
	}

	if zone, ok := params[paramAvailabilityZone]; ok && strings.TrimSpace(zone) != "" {
		return strings.TrimSpace(zone)
	}

	return ""
}

// formatCreateVolumeResponse converts an evs.Volume into a csi.CreateVolumeResponse.
// Volume context stays empty; the attach handoff uses the publish context.
func formatCreateVolumeResponse(vol *evs.Volume) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      vol.ID,
			CapacityBytes: int64(vol.Size) * bytesInGiB,
			AccessibleTopology: []*csi.Topology{
				{
					Segments: map[string]string{
						TopologyZoneKey: vol.AvailabilityZone,
					},
				},
			},
		},
	}
}

// toGRPCError converts package domain errors into canonical gRPC status errors.
//
// conflictCode is the code CSI assigns to a conflict for this operation; it differs per
// operation and sidecars branch on it, so the caller passes it in.
func toGRPCError(op string, conflictCode codes.Code, err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, evs.ErrNotFound):
		return status.Error(codes.NotFound, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, evs.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, evs.ErrConflict):
		return status.Error(conflictCode, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, evs.ErrUnauthenticated):
		return status.Error(codes.Unauthenticated, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, evs.ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, evs.ErrUnavailable):
		return status.Error(codes.Unavailable, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, fmt.Sprintf("%s: %v", op, err))
	default:
		return status.Error(codes.Internal, fmt.Sprintf("%s: %v", op, err))
	}
}
