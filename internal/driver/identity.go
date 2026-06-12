// Package driver implements Container Storage Interface (CSI) gRPC services.
package driver

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/config"
)

// IdentityService implements the csi.IdentityServer interface.
type IdentityService struct {
	csi.UnimplementedIdentityServer

	name    string
	version string
	logger  *slog.Logger
}

func NewIdentityService(cfg *config.Config, logger *slog.Logger) (*IdentityService, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}
	if cfg.DriverName == "" {
		return nil, fmt.Errorf("driver name cannot be empty")
	}
	if cfg.Version == "" {
		return nil, fmt.Errorf("version cannot be empty")
	}
	return &IdentityService{
		name:    cfg.DriverName,
		version: cfg.Version,
		logger:  logger,
	}, nil
}

// GetPluginInfo returns the driver name and version.
func (s *IdentityService) GetPluginInfo(
	ctx context.Context,
	req *csi.GetPluginInfoRequest,
) (*csi.GetPluginInfoResponse, error) {
	if s.name == "" {
		return nil, status.Error(codes.InvalidArgument, "driver name is empty")
	}
	if s.version == "" {
		return nil, status.Error(codes.InvalidArgument, "driver version is empty")
	}
	return &csi.GetPluginInfoResponse{
		Name:          s.name,
		VendorVersion: s.version,
	}, nil
}

// GetPluginCapabilities returns the supported capabilities of the plugin.
func (s *IdentityService) GetPluginCapabilities(
	ctx context.Context,
	req *csi.GetPluginCapabilitiesRequest,
) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
					},
				},
			},
		},
	}, nil
}

// Probe checks the health and readiness of the plugin.
func (s *IdentityService) Probe(
	ctx context.Context,
	req *csi.ProbeRequest,
) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{
		Ready: wrapperspb.Bool(true),
	}, nil
}
