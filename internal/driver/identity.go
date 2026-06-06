// Package driver implements Container Storage Interface (CSI) gRPC services.
package driver

import (
	"context"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
)

// IdentityService implements the csi.IdentityServer interface.
type IdentityService struct {
	csi.UnimplementedIdentityServer

	name    string
	version string
}

// NewIdentityService creates a new IdentityService with the provided configuration.
func NewIdentityService(cfg *config.Config) (*IdentityService, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
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
