package driver_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

type rpcCall struct {
	name   string
	invoke func(context.Context) error
}

func wireRPC[Request, Response any](
	name string,
	invoke func(context.Context, *Request, ...grpc.CallOption) (*Response, error),
) rpcCall {
	return rpcCall{
		name: name,
		invoke: func(ctx context.Context) error {
			_, err := invoke(ctx, new(Request))
			return err
		},
	}
}

type rpcError struct {
	name string
	err  error
}

// identityRPCs lists the implemented Identity RPCs. Together with controllerRPCs and nodeRPCs,
// these lists drive the reachability and unadvertised tests, and the inventory test reconciles
// them against the pinned generated interface.
func identityRPCs(client csi.IdentityClient) (implemented, unadvertised []rpcCall) {
	implemented = []rpcCall{
		wireRPC("GetPluginInfo", client.GetPluginInfo),
		wireRPC("GetPluginCapabilities", client.GetPluginCapabilities),
		wireRPC("Probe", client.Probe),
	}

	return implemented, nil
}

// controllerRPCs lists the implemented and unadvertised Controller RPCs.
func controllerRPCs(client csi.ControllerClient) (implemented, unadvertised []rpcCall) {
	implemented = []rpcCall{
		wireRPC("CreateVolume", client.CreateVolume),
		wireRPC("DeleteVolume", client.DeleteVolume),
		wireRPC("ControllerPublishVolume", client.ControllerPublishVolume),
		wireRPC("ControllerUnpublishVolume", client.ControllerUnpublishVolume),
		wireRPC("ValidateVolumeCapabilities", client.ValidateVolumeCapabilities),
		wireRPC("ControllerGetCapabilities", client.ControllerGetCapabilities),
	}
	unadvertised = []rpcCall{
		wireRPC("ListVolumes", client.ListVolumes),
		wireRPC("ControllerListVolumeHealth", client.ControllerListVolumeHealth),
		wireRPC("ControllerGetVolumeHealth", client.ControllerGetVolumeHealth),
		wireRPC("GetCapacity", client.GetCapacity),
		wireRPC("CreateSnapshot", client.CreateSnapshot),
		wireRPC("DeleteSnapshot", client.DeleteSnapshot),
		wireRPC("ListSnapshots", client.ListSnapshots),
		wireRPC("GetSnapshot", client.GetSnapshot),
		wireRPC("ControllerExpandVolume", client.ControllerExpandVolume),
		wireRPC("ControllerGetVolume", client.ControllerGetVolume),
		wireRPC("ControllerModifyVolume", client.ControllerModifyVolume),
	}

	return implemented, unadvertised
}

// nodeRPCs lists the implemented and unadvertised Node RPCs.
func nodeRPCs(client csi.NodeClient) (implemented, unadvertised []rpcCall) {
	implemented = []rpcCall{
		wireRPC("NodeStageVolume", client.NodeStageVolume),
		wireRPC("NodeUnstageVolume", client.NodeUnstageVolume),
		wireRPC("NodePublishVolume", client.NodePublishVolume),
		wireRPC("NodeUnpublishVolume", client.NodeUnpublishVolume),
		wireRPC("NodeGetCapabilities", client.NodeGetCapabilities),
		wireRPC("NodeGetInfo", client.NodeGetInfo),
	}
	unadvertised = []rpcCall{
		wireRPC("NodeGetVolumeStats", client.NodeGetVolumeStats),
		wireRPC("NodeGetVolumeHealth", client.NodeGetVolumeHealth),
		wireRPC("NodeGetStorageHealth", client.NodeGetStorageHealth),
		wireRPC("NodeExpandVolume", client.NodeExpandVolume),
	}

	return implemented, unadvertised
}

// countingEVSClient records every cloud call reaching the EVS boundary.
type countingEVSClient struct {
	*mockEVSClient
	calls int
}

func (c *countingEVSClient) CreateVolume(
	ctx context.Context,
	opts evs.CreateVolumeOpts,
) (*evs.Volume, error) {
	c.calls++

	return c.mockEVSClient.CreateVolume(ctx, opts)
}

func (c *countingEVSClient) GetVolume(ctx context.Context, id string) (*evs.Volume, error) {
	c.calls++

	return c.mockEVSClient.GetVolume(ctx, id)
}

func (c *countingEVSClient) DiscoverVolume(
	ctx context.Context,
	opts evs.DiscoverVolumeOpts,
) (*evs.Volume, error) {
	c.calls++

	return c.mockEVSClient.DiscoverVolume(ctx, opts)
}

func (c *countingEVSClient) DeleteVolume(ctx context.Context, id string) error {
	c.calls++

	return c.mockEVSClient.DeleteVolume(ctx, id)
}

func (c *countingEVSClient) AttachVolume(
	ctx context.Context,
	volumeID, serverID string,
) (*evs.Attachment, error) {
	c.calls++

	return c.mockEVSClient.AttachVolume(ctx, volumeID, serverID)
}

func (c *countingEVSClient) DetachVolume(ctx context.Context, volumeID, serverID string) error {
	c.calls++

	return c.mockEVSClient.DetachVolume(ctx, volumeID, serverID)
}

// countingMounter returns a fakeMounter whose methods all increment counter.
func countingMounter(counter *int) *fakeMounter {
	return &fakeMounter{
		discoverDeviceFn: func(context.Context, string, string) (string, error) {
			*counter++

			return "", nil
		},
		formatAndMountFn: func(context.Context, string, string, string, []string) error {
			*counter++

			return nil
		},
		mountFn: func(context.Context, string, string, string, []string) error {
			*counter++

			return nil
		},
		unmountFn: func(context.Context, string) error {
			*counter++

			return nil
		},
		isMountPointFn: func(context.Context, string) (bool, error) {
			*counter++

			return false, nil
		},
		getFilesystemTypeFn: func(context.Context, string) (string, error) {
			*counter++

			return "", nil
		},
		getMountSourceFn: func(context.Context, string) (string, error) {
			*counter++

			return "", nil
		},
	}
}

// contractClients serves the real driver services over in-process connections, wired to doubles
// that record every EVS and mounter call the checks cause.
type contractClients struct {
	identity   csi.IdentityClient
	controller csi.ControllerClient
	node       csi.NodeClient
	cloud      *countingEVSClient
	hostCalls  *int
}

func newContractClients(t *testing.T) contractClients {
	t.Helper()

	cfg := validTestConfig()
	cloud := &countingEVSClient{mockEVSClient: newMockEVSClient()}
	hostCalls := new(int)

	identityService, err := driver.NewIdentityService(cfg)
	if err != nil {
		t.Fatalf("NewIdentityService failed: %v", err)
	}
	controllerService, err := driver.NewControllerService(cloud, cfg)
	if err != nil {
		t.Fatalf("NewControllerService failed: %v", err)
	}
	nodeService, err := driver.NewNodeService(countingMounter(hostCalls), cfg)
	if err != nil {
		t.Fatalf("NewNodeService failed: %v", err)
	}

	return contractClients{
		identity:   newIdentityClient(t, identityService),
		controller: newControllerClient(t, controllerService),
		node:       newNodeClient(t, nodeService),
		cloud:      cloud,
		hostCalls:  hostCalls,
	}
}

// assertNoEffects fails if any contract check reached the EVS or mounter boundary. The Node
// service also creates and removes mount paths with direct os calls that bypass the mounter and
// are not covered here.
func (c contractClients) assertNoEffects(t *testing.T) {
	t.Helper()

	if c.cloud.calls != 0 {
		t.Errorf(
			"expected no EVS call from the contract checks, got %d",
			c.cloud.calls,
		)
	}
	if *c.hostCalls != 0 {
		t.Errorf(
			"expected no mounter call from the contract checks, got %d",
			*c.hostCalls,
		)
	}
}

// implementedStatusErrors invokes every implemented Identity, Controller and Node RPC with an
// empty request and reports each one that returns codes.Unimplemented, which means the service
// stopped overriding it and the embedded generated stub is answering instead. Any other status
// counts as reachable, including the codes.InvalidArgument an empty request usually returns, so
// the check does not depend on each RPC's request validation.
func implementedStatusErrors(
	ctx context.Context,
	identityClient csi.IdentityClient,
	controllerClient csi.ControllerClient,
	nodeClient csi.NodeClient,
) []rpcError {
	identityImplemented, _ := identityRPCs(identityClient)
	controllerImplemented, _ := controllerRPCs(controllerClient)
	nodeImplemented, _ := nodeRPCs(nodeClient)

	services := []struct {
		name  string
		calls []rpcCall
	}{
		{"Identity", identityImplemented},
		{"Controller", controllerImplemented},
		{"Node", nodeImplemented},
	}

	var failures []rpcError
	for _, service := range services {
		for _, call := range service.calls {
			if err := call.invoke(ctx); status.Code(err) == codes.Unimplemented {
				failures = append(failures, rpcError{
					name: service.name + "/" + call.name,
					err:  fmt.Errorf("RPC listed as implemented is not reachable: %v", err),
				})
			}
		}
	}

	return failures
}

// unimplementedStatusErrors invokes every unadvertised Controller and Node RPC with an empty
// request and reports each one that does not return codes.Unimplemented.
func unimplementedStatusErrors(
	ctx context.Context,
	controllerClient csi.ControllerClient,
	nodeClient csi.NodeClient,
) []rpcError {
	_, controllerUnadvertised := controllerRPCs(controllerClient)
	_, nodeUnadvertised := nodeRPCs(nodeClient)

	services := []struct {
		name  string
		calls []rpcCall
	}{
		{"Controller", controllerUnadvertised},
		{"Node", nodeUnadvertised},
	}

	var failures []rpcError
	for _, service := range services {
		for _, call := range service.calls {
			err := call.invoke(ctx)
			if code := status.Code(err); code != codes.Unimplemented {
				failures = append(failures, rpcError{
					name: service.name + "/" + call.name,
					err:  fmt.Errorf("expected codes.Unimplemented, got %s: %v", code, err),
				})
			}
		}
	}

	return failures
}

// advertisedCapabilityErrors maps each advertised plugin, Controller and Node capability to the
// RPCs it requires and reports unknown capabilities and required RPCs that return
// codes.Unimplemented.
func advertisedCapabilityErrors(
	ctx context.Context,
	identityClient csi.IdentityClient,
	controllerClient csi.ControllerClient,
	nodeClient csi.NodeClient,
) []rpcError {
	var failures []rpcError
	var advertised []rpcCall

	pluginResponse, err := identityClient.GetPluginCapabilities(
		ctx,
		&csi.GetPluginCapabilitiesRequest{},
	)
	if err != nil {
		failures = append(failures, rpcError{
			name: "GetPluginCapabilities",
			err:  fmt.Errorf("GetPluginCapabilities failed: %v", err),
		})
	} else {
		for _, capability := range pluginResponse.GetCapabilities() {
			switch capability.GetService().GetType() {
			case csi.PluginCapability_Service_CONTROLLER_SERVICE:
				advertised = append(advertised,
					wireRPC(
						"CONTROLLER_SERVICE/ControllerGetCapabilities",
						controllerClient.ControllerGetCapabilities,
					),
					wireRPC(
						"CONTROLLER_SERVICE/ValidateVolumeCapabilities",
						controllerClient.ValidateVolumeCapabilities,
					),
				)
			case csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS:
				advertised = append(advertised,
					wireRPC(
						"VOLUME_ACCESSIBILITY_CONSTRAINTS/CreateVolume",
						controllerClient.CreateVolume,
					),
					wireRPC(
						"VOLUME_ACCESSIBILITY_CONSTRAINTS/NodeGetInfo",
						nodeClient.NodeGetInfo,
					),
				)
			default:
				failures = append(failures, rpcError{
					name: "Identity/unknown capability",
					err:  fmt.Errorf("plugin advertises unsupported capability %v", capability),
				})
			}
		}
	}

	controllerResponse, err := controllerClient.ControllerGetCapabilities(
		ctx,
		&csi.ControllerGetCapabilitiesRequest{},
	)
	if err != nil {
		failures = append(failures, rpcError{
			name: "ControllerGetCapabilities",
			err:  fmt.Errorf("ControllerGetCapabilities failed: %v", err),
		})
	} else {
		for _, capability := range controllerResponse.GetCapabilities() {
			switch capability.GetRpc().GetType() {
			case csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME:
				advertised = append(advertised,
					wireRPC("CREATE_DELETE_VOLUME/CreateVolume", controllerClient.CreateVolume),
					wireRPC("CREATE_DELETE_VOLUME/DeleteVolume", controllerClient.DeleteVolume),
				)
			case csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME:
				advertised = append(advertised,
					wireRPC(
						"PUBLISH_UNPUBLISH_VOLUME/ControllerPublishVolume",
						controllerClient.ControllerPublishVolume,
					),
					wireRPC(
						"PUBLISH_UNPUBLISH_VOLUME/ControllerUnpublishVolume",
						controllerClient.ControllerUnpublishVolume,
					),
					wireRPC(
						"PUBLISH_UNPUBLISH_VOLUME/NodePublishVolume",
						nodeClient.NodePublishVolume,
					),
					wireRPC(
						"PUBLISH_UNPUBLISH_VOLUME/NodeUnpublishVolume",
						nodeClient.NodeUnpublishVolume,
					),
					wireRPC("PUBLISH_UNPUBLISH_VOLUME/NodeGetInfo", nodeClient.NodeGetInfo),
				)
			default:
				failures = append(failures, rpcError{
					name: "Controller/unknown capability",
					err:  fmt.Errorf("Controller advertises unsupported capability %v", capability),
				})
			}
		}
	}

	nodeResponse, err := nodeClient.NodeGetCapabilities(ctx, &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		failures = append(failures, rpcError{
			name: "NodeGetCapabilities",
			err:  fmt.Errorf("NodeGetCapabilities failed: %v", err),
		})
	} else {
		for _, capability := range nodeResponse.GetCapabilities() {
			switch capability.GetRpc().GetType() {
			case csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME:
				advertised = append(advertised,
					wireRPC("STAGE_UNSTAGE_VOLUME/NodeStageVolume", nodeClient.NodeStageVolume),
					wireRPC("STAGE_UNSTAGE_VOLUME/NodeUnstageVolume", nodeClient.NodeUnstageVolume),
				)
			default:
				failures = append(failures, rpcError{
					name: "Node/unknown capability",
					err:  fmt.Errorf("Node advertises unsupported capability %v", capability),
				})
			}
		}
	}

	for _, rpc := range advertised {
		if err := rpc.invoke(ctx); status.Code(err) == codes.Unimplemented {
			failures = append(failures, rpcError{
				name: rpc.name,
				err:  fmt.Errorf("advertised capability maps to an unimplemented RPC: %v", err),
			})
		}
	}

	return failures
}

func TestUnadvertisedRPCsReturnUnimplementedOverGRPC(t *testing.T) {
	t.Parallel()

	clients := newContractClients(t)

	for _, failure := range unimplementedStatusErrors(
		t.Context(),
		clients.controller,
		clients.node,
	) {
		t.Errorf("%s: %v", failure.name, failure.err)
	}

	clients.assertNoEffects(t)
}

func TestImplementedRPCsAreReachableOverGRPC(t *testing.T) {
	t.Parallel()

	clients := newContractClients(t)

	for _, failure := range implementedStatusErrors(
		t.Context(),
		clients.identity,
		clients.controller,
		clients.node,
	) {
		t.Errorf("%s: %v", failure.name, failure.err)
	}

	clients.assertNoEffects(t)
}

func TestAdvertisedCapabilitiesHaveImplementedRPCs(t *testing.T) {
	t.Parallel()

	clients := newContractClients(t)

	for _, failure := range advertisedCapabilityErrors(
		t.Context(),
		clients.identity,
		clients.controller,
		clients.node,
	) {
		t.Errorf("%s: %v", failure.name, failure.err)
	}

	clients.assertNoEffects(t)
}

// TestGeneratedInterfaceInventoryIsCovered partitions every RPC of the pinned generated Identity,
// Controller and Node interfaces into implemented and unadvertised sets, so a spec revision bump
// that adds an RPC fails here until the contract above classifies it. Unadvertised services outside
// Identity, Controller and Node are out of scope.
func TestGeneratedInterfaceInventoryIsCovered(t *testing.T) {
	t.Parallel()

	clients := newContractClients(t)
	implementedIdentity, unadvertisedIdentity := identityRPCs(clients.identity)
	implementedController, unadvertisedController := controllerRPCs(clients.controller)
	implementedNode, unadvertisedNode := nodeRPCs(clients.node)

	services := []struct {
		name         string
		desc         grpc.ServiceDesc
		implemented  []rpcCall
		unadvertised []rpcCall
	}{
		{"Identity", csi.Identity_ServiceDesc, implementedIdentity, unadvertisedIdentity},
		{
			"Controller",
			csi.Controller_ServiceDesc,
			implementedController,
			unadvertisedController,
		},
		{"Node", csi.Node_ServiceDesc, implementedNode, unadvertisedNode},
	}

	for _, service := range services {
		classified := make(map[string]bool, len(service.implemented)+len(service.unadvertised))
		for _, list := range [][]rpcCall{service.implemented, service.unadvertised} {
			for _, call := range list {
				if classified[call.name] {
					t.Errorf(
						"%s/%s is classified more than once",
						service.name,
						call.name,
					)
				}
				classified[call.name] = true
			}
		}

		for _, method := range service.desc.Methods {
			if !classified[method.MethodName] {
				t.Errorf(
					"%s/%s is in the generated interface but is neither implemented "+
						"nor asserted unadvertised",
					service.name,
					method.MethodName,
				)
			}
			delete(classified, method.MethodName)
		}
		for _, name := range slices.Sorted(maps.Keys(classified)) {
			t.Errorf(
				"%s/%s is classified but not present in the generated interface",
				service.name,
				name,
			)
		}

		// wireRPC only binds unary calls, so a streaming RPC cannot be classified above and
		// would otherwise pass this inventory unnoticed.
		for _, stream := range service.desc.Streams {
			t.Errorf(
				"%s/%s is a streaming RPC; this contract covers only unary RPCs and needs "+
					"extending to cover it",
				service.name,
				stream.StreamName,
			)
		}
	}
}

// wrongStatusController answers one unadvertised RPC with the wrong status code.
type wrongStatusController struct {
	csi.UnimplementedControllerServer
}

func (wrongStatusController) GetCapacity(
	context.Context,
	*csi.GetCapacityRequest,
) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Internal, "simulated defect")
}

// TestUnadvertisedCheckDetectsAWrongStatusCode verifies the unadvertised-RPC check fails when an
// unsupported RPC returns a status other than codes.Unimplemented.
func TestUnadvertisedCheckDetectsAWrongStatusCode(t *testing.T) {
	t.Parallel()

	controllerClient := newControllerClient(t, wrongStatusController{})
	nodeClient := newNodeClient(t, csi.UnimplementedNodeServer{})

	failures := unimplementedStatusErrors(t.Context(), controllerClient, nodeClient)
	if len(failures) != 1 || failures[0].name != "Controller/GetCapacity" {
		t.Errorf(
			"expected exactly the Controller/GetCapacity defect to be reported, got %v",
			failures,
		)
	}
}

// unimplementedRequiredRPCController advertises a Controller capability while leaving the RPCs it
// requires unimplemented.
type unimplementedRequiredRPCController struct {
	csi.UnimplementedControllerServer
}

func (unimplementedRequiredRPCController) ControllerGetCapabilities(
	context.Context,
	*csi.ControllerGetCapabilitiesRequest,
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
		},
	}, nil
}

// TestCapabilityCheckDetectsAnUnimplementedRequiredRPC verifies the capability check fails when a
// service advertises a capability whose required RPC returns codes.Unimplemented.
func TestCapabilityCheckDetectsAnUnimplementedRequiredRPC(t *testing.T) {
	t.Parallel()

	clients := newContractClients(t)
	fakeController := newControllerClient(t, unimplementedRequiredRPCController{})

	failures := advertisedCapabilityErrors(
		t.Context(),
		clients.identity,
		fakeController,
		clients.node,
	)

	found := slices.ContainsFunc(failures, func(failure rpcError) bool {
		return failure.name == "CREATE_DELETE_VOLUME/CreateVolume"
	})
	if !found {
		t.Errorf(
			"expected the unimplemented CreateVolume required by the advertised capability "+
				"to be reported, got %v",
			failures,
		)
	}

	clients.assertNoEffects(t)
}

// unknownCapabilityNode advertises a Node capability outside the driver's contract.
type unknownCapabilityNode struct {
	csi.UnimplementedNodeServer
}

func (unknownCapabilityNode) NodeGetCapabilities(
	context.Context,
	*csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
		},
	}, nil
}

// TestCapabilityCheckDetectsAnUnknownCapability verifies the capability check fails when a
// service advertises a capability the contract does not know. The fake also leaves the other Node
// RPCs unimplemented, which adds failures of its own, so the check searches the failures instead
// of matching them exactly.
func TestCapabilityCheckDetectsAnUnknownCapability(t *testing.T) {
	t.Parallel()

	clients := newContractClients(t)
	fakeNode := newNodeClient(t, unknownCapabilityNode{})

	failures := advertisedCapabilityErrors(
		t.Context(),
		clients.identity,
		clients.controller,
		fakeNode,
	)

	found := slices.ContainsFunc(failures, func(failure rpcError) bool {
		return failure.name == "Node/unknown capability"
	})
	if !found {
		t.Errorf("expected the unknown Node capability to be reported, got %v", failures)
	}

	clients.assertNoEffects(t)
}

// TestImplementedCheckDetectsAnUnreachableRPC verifies the reachability check fails when a service
// stops overriding an RPC the contract lists as implemented, so the embedded generated stub
// answers with codes.Unimplemented instead.
func TestImplementedCheckDetectsAnUnreachableRPC(t *testing.T) {
	t.Parallel()

	clients := newContractClients(t)
	bareIdentity := newIdentityClient(t, csi.UnimplementedIdentityServer{})

	failures := implementedStatusErrors(
		t.Context(),
		bareIdentity,
		clients.controller,
		clients.node,
	)

	found := slices.ContainsFunc(failures, func(failure rpcError) bool {
		return failure.name == "Identity/Probe"
	})
	if !found {
		t.Errorf("expected the unreachable Probe to be reported, got %v", failures)
	}

	clients.assertNoEffects(t)
}
