// Package driver implements Container Storage Interface (CSI) gRPC services.
package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

const (
	// TopologyZoneKey is the standard CSI topology key for availability zones.
	TopologyZoneKey = "topology.kubernetes.io/zone"
	// DefaultVolumeType is the default EVS volume type used when none is specified.
	DefaultVolumeType = "SSD"
	// DefaultVolumeSizeGiB is the default volume size in GiB when capacity is not specified.
	DefaultVolumeSizeGiB = 1
	// bytesInGiB is the number of bytes in one GiB.
	bytesInGiB = 1024 * 1024 * 1024
)

// EVSClient defines the subset of EVS operations required by the CSI Controller service.
type EVSClient interface {
	CreateVolume(ctx context.Context, opts evs.CreateVolumeOpts) (*evs.Volume, error)
	GetVolume(ctx context.Context, id string) (*evs.Volume, error)
	ListVolumes(ctx context.Context, opts evs.ListVolumeOpts) ([]evs.Volume, error)
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

	err := s.evsClient.AttachVolume(ctx, req.GetVolumeId(), req.GetNodeId())
	if err != nil {
		return nil, toGRPCError(
			fmt.Sprintf("publish volume %q to node %q", req.GetVolumeId(), req.GetNodeId()),
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

	for _, cap := range req.GetVolumeCapabilities() {
		if err := validateVolumeCapability(cap); err != nil {
			return nil, err
		}
	}

	sizeGiB, err := calculateSizeGiB(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}

	zone := pickAvailabilityZone(req.GetAccessibilityRequirements(), req.GetParameters())
	if zone == "" {
		return nil, status.Error(
			codes.InvalidArgument,
			"availability zone is required in topology requirements or parameters",
		)
	}

	volType := strings.TrimSpace(req.GetParameters()["type"])
	if volType == "" {
		volType = strings.TrimSpace(req.GetParameters()["volume_type"])
	}
	if volType == "" {
		volType = DefaultVolumeType
	}

	existing, err := s.evsClient.ListVolumes(ctx, evs.ListVolumeOpts{
		Name: req.GetName(),
	})
	if err != nil {
		return nil, toGRPCError("list volumes for idempotency check", err)
	}

	for _, vol := range existing {
		if vol.Name == req.GetName() {
			if vol.Size < sizeGiB {
				return nil, status.Error(
					codes.AlreadyExists,
					fmt.Sprintf(
						"volume %q already exists with size %d GiB, smaller than requested %d GiB",
						req.GetName(),
						vol.Size,
						sizeGiB,
					),
				)
			}
			return formatCreateVolumeResponse(&vol, req.GetParameters()), nil
		}
	}

	opts := evs.CreateVolumeOpts{
		Name:             req.GetName(),
		Size:             sizeGiB,
		AvailabilityZone: zone,
		VolumeType:       volType,
		Metadata:         req.GetParameters(),
	}

	created, err := s.evsClient.CreateVolume(ctx, opts)
	if err != nil {
		return nil, toGRPCError(fmt.Sprintf("create volume %q", req.GetName()), err)
	}

	return formatCreateVolumeResponse(created, req.GetParameters()), nil
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
		return nil, toGRPCError(fmt.Sprintf("delete volume %q", req.GetVolumeId()), err)
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
		return nil, toGRPCError(fmt.Sprintf("get volume %q", req.GetVolumeId()), err)
	}

	for _, cap := range req.GetVolumeCapabilities() {
		if err := validateVolumeCapability(cap); err != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Confirmed: nil,
				Message:   "unsupported volume capability or access mode",
			}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

// validateVolumeCapability validates that a volume uses a supported single-node access mode.
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
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		return nil
	default:
		return status.Error(
			codes.InvalidArgument,
			fmt.Sprintf(
				"unsupported access mode %s: only single-node modes supported",
				accessMode.GetMode(),
			),
		)
	}
}

// calculateSizeGiB converts a CSI CapacityRange to size in GiB, enforcing minimums and limits.
func calculateSizeGiB(capRange *csi.CapacityRange) (int, error) {
	if capRange == nil {
		return DefaultVolumeSizeGiB, nil
	}

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

	if reqBytes == 0 {
		reqBytes = int64(DefaultVolumeSizeGiB) * bytesInGiB
	}

	sizeGiB := int((reqBytes + bytesInGiB - 1) / bytesInGiB)
	if sizeGiB < 1 {
		sizeGiB = 1
	}

	if limitBytes > 0 && int64(sizeGiB)*bytesInGiB > limitBytes {
		return 0, status.Error(
			codes.InvalidArgument,
			fmt.Sprintf("calculated size (%d GiB) exceeds limit_bytes (%d)", sizeGiB, limitBytes),
		)
	}

	return sizeGiB, nil
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

	if zone, ok := params["availability_zone"]; ok && strings.TrimSpace(zone) != "" {
		return strings.TrimSpace(zone)
	}
	if zone, ok := params["zone"]; ok && strings.TrimSpace(zone) != "" {
		return strings.TrimSpace(zone)
	}

	return ""
}

// formatCreateVolumeResponse converts an evs.Volume struct into a csi.CreateVolumeResponse.
func formatCreateVolumeResponse(
	vol *evs.Volume,
	params map[string]string,
) *csi.CreateVolumeResponse {
	volumeContext := make(map[string]string)
	for k, v := range params {
		volumeContext[k] = v
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      vol.ID,
			CapacityBytes: int64(vol.Size) * bytesInGiB,
			VolumeContext: volumeContext,
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
func toGRPCError(op string, err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, evs.ErrNotFound):
		return status.Error(codes.NotFound, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, evs.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, fmt.Sprintf("%s: %v", op, err))
	case errors.Is(err, evs.ErrConflict):
		return status.Error(codes.AlreadyExists, fmt.Sprintf("%s: %v", op, err))
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
