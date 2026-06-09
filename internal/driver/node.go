// Package driver implements Container Storage Interface (CSI) gRPC services.
package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"wilaris.dev/t-cloud-public-csi-driver/internal/mount"
)

const (
	// devicePathKey is the publish context key carrying the device path observed at attach time.
	devicePathKey = "devicePath"
	// mountPathPerm is the permission mode for driver-created staging and target directories.
	// After a successful mount the visible mode comes from the mounted filesystem instead.
	mountPathPerm = 0o750
	// blockTargetFilePerm is the permission mode for the placeholder file that a raw block
	// device is bind mounted onto. After the mount the device node's own mode applies.
	blockTargetFilePerm = 0o600
)

// NodeService implements the csi.NodeServer interface.
type NodeService struct {
	csi.UnimplementedNodeServer

	mounter mount.Mounter
	cfg     *config.Config
}

// NewNodeService constructs a new NodeService instance.
func NewNodeService(mounter mount.Mounter, cfg *config.Config) (*NodeService, error) {
	if mounter == nil {
		return nil, fmt.Errorf("mounter cannot be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("node ID cannot be empty")
	}
	if cfg.AvailabilityZone == "" {
		return nil, fmt.Errorf("availability zone cannot be empty")
	}
	return &NodeService{
		mounter: mounter,
		cfg:     cfg,
	}, nil
}

// NodeGetInfo returns the node ID (compute instance Server UUID) and availability zone topology.
//
// The reported zone must be the availability zone the node's EVS volumes are attachable in.
// A region covers several zones, so reporting one in place of the other would let Kubernetes
// schedule a workload onto a node that cannot reach its volume.
func (s *NodeService) NodeGetInfo(
	ctx context.Context,
	req *csi.NodeGetInfoRequest,
) (*csi.NodeGetInfoResponse, error) {
	if s.cfg.NodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "node ID is not configured")
	}
	if s.cfg.AvailabilityZone == "" {
		return nil, status.Error(codes.FailedPrecondition, "availability zone is not configured")
	}

	return &csi.NodeGetInfoResponse{
		NodeId: s.cfg.NodeID,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				TopologyZoneKey: s.cfg.AvailabilityZone,
			},
		},
	}, nil
}

// NodeGetCapabilities returns supported capabilities for the Node service.
func (s *NodeService) NodeGetCapabilities(
	ctx context.Context,
	req *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
		},
	}, nil
}

// NodeStageVolume formats (if necessary) and mounts the block device to a global staging directory.
func (s *NodeService) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id cannot be empty")
	}
	if strings.TrimSpace(req.GetStagingTargetPath()) == "" {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path cannot be empty")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume_capability cannot be nil")
	}

	if err := validateVolumeCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}

	stagingPath := req.GetStagingTargetPath()

	sourceDevice, err := s.discoverDevice(ctx, req.GetVolumeId(), req.GetPublishContext())
	if err != nil {
		return nil, err
	}

	switch req.GetVolumeCapability().GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		if err := os.MkdirAll(stagingPath, mountPathPerm); err != nil {
			return nil, status.Error(
				codes.Internal,
				fmt.Sprintf("failed to create staging directory %s: %v", stagingPath, err),
			)
		}
		return &csi.NodeStageVolumeResponse{}, nil

	case *csi.VolumeCapability_Mount:
		mountCap := req.GetVolumeCapability().GetMount()
		fsType := mountCap.GetFsType()
		if fsType == "" {
			fsType = mount.DefaultFilesystemType
		}
		mountOptions := mountCap.GetMountFlags()

		err := s.mounter.FormatAndMount(
			ctx,
			sourceDevice,
			stagingPath,
			fsType,
			mountOptions,
		)
		if err != nil {
			return nil, status.Error(
				codes.Internal,
				fmt.Sprintf(
					"failed to format and mount device %s at %s: %v",
					sourceDevice,
					stagingPath,
					err,
				),
			)
		}
		return &csi.NodeStageVolumeResponse{}, nil

	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported volume access type")
	}
}

// NodeUnstageVolume unmounts the volume from the global staging directory.
func (s *NodeService) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id cannot be empty")
	}
	if strings.TrimSpace(req.GetStagingTargetPath()) == "" {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path cannot be empty")
	}

	stagingPath := req.GetStagingTargetPath()

	if err := s.mounter.Unmount(ctx, stagingPath); err != nil {
		return nil, status.Error(
			codes.Internal,
			fmt.Sprintf(
				"failed to unstage volume %q at %s: %v",
				req.GetVolumeId(),
				stagingPath,
				err,
			),
		)
	}

	if err := removeMountPath(stagingPath); err != nil {
		return nil, err
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume bind mounts the volume from staging (or device) to the target path.
func (s *NodeService) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id cannot be empty")
	}
	if strings.TrimSpace(req.GetStagingTargetPath()) == "" {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path cannot be empty")
	}
	if strings.TrimSpace(req.GetTargetPath()) == "" {
		return nil, status.Error(codes.InvalidArgument, "target_path cannot be empty")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume_capability cannot be nil")
	}

	if err := validateVolumeCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}

	// Reject a writable publish for a read-only mode before touching the host.
	readOnlyMode := req.GetVolumeCapability().GetAccessMode().GetMode() ==
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY
	if readOnlyMode && !req.GetReadonly() {
		return nil, status.Error(
			codes.InvalidArgument,
			"writable publication requested for a read-only access mode",
		)
	}

	stagingPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()

	// Publishing is always a bind mount; the source is never probed or formatted here.
	mountOptions := []string{"bind"}
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	switch req.GetVolumeCapability().GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		sourceDevice, err := s.discoverDevice(ctx, req.GetVolumeId(), req.GetPublishContext())
		if err != nil {
			return nil, err
		}

		if err := createBlockTargetFile(targetPath); err != nil {
			return nil, err
		}

		if err := s.mounter.Mount(ctx, sourceDevice, targetPath, "", mountOptions); err != nil {
			return nil, status.Error(
				codes.Internal,
				fmt.Sprintf(
					"failed to bind mount raw block device %s to %s: %v",
					sourceDevice,
					targetPath,
					err,
				),
			)
		}
		return &csi.NodePublishVolumeResponse{}, nil

	case *csi.VolumeCapability_Mount:
		staged, err := s.mounter.IsMountPoint(ctx, stagingPath)
		if err != nil {
			return nil, status.Error(
				codes.Internal,
				fmt.Sprintf("failed to check staging path %s: %v", stagingPath, err),
			)
		}
		if !staged {
			return nil, status.Error(
				codes.FailedPrecondition,
				fmt.Sprintf(
					"volume %q is not staged at %s",
					req.GetVolumeId(),
					stagingPath,
				),
			)
		}

		if err := os.MkdirAll(targetPath, mountPathPerm); err != nil {
			return nil, status.Error(
				codes.Internal,
				fmt.Sprintf("failed to create target directory %s: %v", targetPath, err),
			)
		}

		for _, opt := range req.GetVolumeCapability().GetMount().GetMountFlags() {
			if opt != "bind" && opt != "ro" {
				mountOptions = append(mountOptions, opt)
			}
		}

		if err := s.mounter.Mount(ctx, stagingPath, targetPath, "", mountOptions); err != nil {
			return nil, status.Error(
				codes.Internal,
				fmt.Sprintf(
					"failed to bind mount staging path %s at target %s: %v",
					stagingPath,
					targetPath,
					err,
				),
			)
		}
		return &csi.NodePublishVolumeResponse{}, nil

	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported volume access type")
	}
}

// NodeUnpublishVolume unmounts the volume from the target path.
func (s *NodeService) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request cannot be nil")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id cannot be empty")
	}
	if strings.TrimSpace(req.GetTargetPath()) == "" {
		return nil, status.Error(codes.InvalidArgument, "target_path cannot be empty")
	}

	targetPath := req.GetTargetPath()

	if err := s.mounter.Unmount(ctx, targetPath); err != nil {
		return nil, status.Error(
			codes.Internal,
			fmt.Sprintf(
				"failed to unpublish volume %q at target %s: %v",
				req.GetVolumeId(),
				targetPath,
				err,
			),
		)
	}

	if err := removeMountPath(targetPath); err != nil {
		return nil, err
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// discoverDevice resolves the block device backing a volume on this node. The device path
// observed at attach time and passed through the publish context is preferred, with the
// volume ID as a fallback for a controller that does not report one.
func (s *NodeService) discoverDevice(
	ctx context.Context,
	volumeID string,
	publishContext map[string]string,
) (string, error) {
	identifiers := make([]string, 0, 2)
	if devicePath := strings.TrimSpace(publishContext[devicePathKey]); devicePath != "" {
		identifiers = append(identifiers, devicePath)
	}
	identifiers = append(identifiers, volumeID)

	var lastErr error
	for _, identifier := range identifiers {
		device, err := s.mounter.DiscoverDevice(ctx, identifier)
		if err == nil {
			return device, nil
		}
		lastErr = err
	}

	return "", status.Error(
		codes.NotFound,
		fmt.Sprintf("failed to discover block device for volume %q: %v", volumeID, lastErr),
	)
}

// createBlockTargetFile creates the placeholder file that a raw block device is bind
// mounted onto, together with its parent directory.
func createBlockTargetFile(targetPath string) error {
	parentDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(parentDir, mountPathPerm); err != nil {
		return status.Error(
			codes.Internal,
			fmt.Sprintf("failed to create parent directory %s: %v", parentDir, err),
		)
	}

	f, err := os.OpenFile( //nolint:gosec // target path is supplied by the orchestrator
		targetPath,
		os.O_CREATE|os.O_RDWR,
		blockTargetFilePerm,
	)
	if err != nil {
		return status.Error(
			codes.Internal,
			fmt.Sprintf("failed to create target file %s: %v", targetPath, err),
		)
	}
	if err := f.Close(); err != nil {
		return status.Error(
			codes.Internal,
			fmt.Sprintf("failed to close target file %s: %v", targetPath, err),
		)
	}

	return nil
}

// removeMountPath removes an empty staging or target path. Non-recursive on purpose: leftover
// content means the unmount didn't take effect, and deleting it would destroy volume data.
func removeMountPath(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return status.Error(
			codes.Internal,
			fmt.Sprintf("failed to remove mount path %s: %v", path, err),
		)
	}

	return nil
}
