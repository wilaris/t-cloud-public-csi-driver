package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"wilaris.dev/t-cloud-public-csi-driver/internal/config"
)

const (
	testRegion = "eu-de"
	bufSize    = 1024 * 1024
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// setArgs replaces os.Args for the duration of the test. Not safe with t.Parallel.
func setArgs(t *testing.T, args ...string) {
	t.Helper()

	original := os.Args
	os.Args = append([]string{"t-cloud-csi-driver"}, args...)
	t.Cleanup(func() { os.Args = original })
}

// setCloudEnv installs fixture cloud credentials pointing at authURL.
func setCloudEnv(t *testing.T, authURL string) {
	t.Helper()

	t.Setenv(config.EnvAuthURL, authURL)
	t.Setenv(config.EnvAccessKey, "test-access-key")
	t.Setenv(config.EnvSecretKey, "test-secret-key")
	t.Setenv(config.EnvProjectID, "test-project-id")
	t.Setenv(config.EnvRegionName, testRegion)
	t.Setenv(config.EnvSecurityToken, "")
}

// clearCloudEnv unsets all cloud credential env vars.
func clearCloudEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		config.EnvAuthURL,
		config.EnvAccessKey,
		config.EnvSecretKey,
		config.EnvProjectID,
		config.EnvRegionName,
		config.EnvSecurityToken,
	} {
		t.Setenv(key, "")
	}
}

// refusedAuthURL returns a local URL with nothing listening, so dial fails immediately.
func refusedAuthURL(t *testing.T) string {
	t.Helper()

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve a local port: %v", err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()

	return "http://" + address
}

// testSocketEndpoint returns a unix endpoint in a fresh temporary directory.
func testSocketEndpoint(t *testing.T) (endpoint, socketPath string) {
	t.Helper()

	socketPath = filepath.Join(t.TempDir(), "csi.sock")
	return "unix://" + socketPath, socketPath
}

func nodeTestConfig() *config.Config {
	return &config.Config{
		Role:             config.RoleNode,
		Endpoint:         "unix:///tmp/csi.sock",
		NodeID:           "12345678-1234-1234-1234-123456789012",
		AvailabilityZone: "eu-de-01",
		DriverName:       config.DefaultDriverName,
		Version:          config.DriverVersion,
	}
}

// serveRegistered registers services for cfg and returns a client connection to them.
func serveRegistered(t *testing.T, cfg *config.Config) *grpc.ClientConn {
	t.Helper()

	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	if err := registerServices(t.Context(), server, cfg, discardLogger()); err != nil {
		t.Fatalf("expected service registration to succeed, got: %v", err)
	}

	go func() { _ = server.Serve(listener) }()

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	})

	return conn
}

// assertUnknownService requires the failure of an unregistered service, which gRPC reports as
// Unimplemented with an "unknown service" message. A registered service with an unimplemented
// method carries the same code but names the method, so the message is the distinguishing part.
func assertUnknownService(t *testing.T, err error) {
	t.Helper()

	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented for an unregistered service, got: %v", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "unknown service") {
		t.Errorf("expected the service to be unregistered, not merely unimplemented: %v", err)
	}
}

func TestRunRefusesAMissingOrUnknownRole(t *testing.T) {
	// Not parallel: mutates process arguments and environment.
	cases := []struct {
		name        string
		args        []string
		wantInError string
	}{
		{"absent role", nil, "--role"},
		{"unknown role", []string{"--role", "supervisor"}, "supervisor"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Point credentials at a closed port so a later-stage failure cannot mask a bad role.
			setCloudEnv(t, refusedAuthURL(t))
			endpoint, socketPath := testSocketEndpoint(t)
			setArgs(t, append(tc.args, "--endpoint", endpoint)...)

			err := run(discardLogger())
			if err == nil {
				t.Fatal("expected startup to fail without a valid role")
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("expected the error to name the role problem, got: %v", err)
			}
			if _, statErr := os.Stat(socketPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused role left a listener socket at %s", socketPath)
			}
		})
	}
}

func TestRunControllerDependencyFailureLeavesNoSocket(t *testing.T) {
	// Not parallel: mutates process arguments and environment.
	setCloudEnv(t, refusedAuthURL(t))
	endpoint, socketPath := testSocketEndpoint(t)
	setArgs(t, "--role", "controller", "--endpoint", endpoint)

	err := run(discardLogger())
	if err == nil {
		t.Fatal("expected startup to fail when the identity endpoint is unreachable")
	}
	if !strings.Contains(err.Error(), "failed to construct EVS client") {
		t.Errorf("expected the failure to come from EVS client construction, got: %v", err)
	}
	if _, statErr := os.Stat(socketPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a failed startup left a listener socket at %s", socketPath)
	}
}

func TestRunNodeRoleStartsWithoutCloudVariablesAndStopsOnSignal(t *testing.T) {
	// Not parallel: mutates process arguments and environment and signals the process.
	const explicitNodeID = "12345678-1234-1234-1234-123456789012"

	clearCloudEnv(t)
	endpoint, socketPath := testSocketEndpoint(t)
	setArgs(t,
		"--role", "node",
		"--nodeid", explicitNodeID,
		"--availability-zone", "eu-de-01",
		"--endpoint", endpoint,
	)

	stopped := make(chan error, 1)
	go func() { stopped <- run(discardLogger()) }()

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to create socket client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	waitUntilServing(t, conn, stopped)

	info, err := csi.NewNodeClient(conn).NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("expected the node service to answer, got: %v", err)
	}
	if info.GetNodeId() != explicitNodeID {
		t.Errorf("expected the explicit node ID %q, got %q", explicitNodeID, info.GetNodeId())
	}

	_, err = csi.NewControllerClient(conn).ControllerGetCapabilities(
		t.Context(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	assertUnknownService(t, err)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("failed to signal the process: %v", err)
	}

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("expected the signaled process to stop cleanly, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the signaled process did not stop")
	}
}

// waitUntilServing polls Identity Probe until the endpoint answers or startup fails.
func waitUntilServing(t *testing.T, conn *grpc.ClientConn, stopped <-chan error) {
	t.Helper()

	identity := csi.NewIdentityClient(conn)
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-stopped:
			t.Fatalf("startup failed before serving: %v", err)
		default:
		}

		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		_, err := identity.Probe(ctx, &csi.ProbeRequest{})
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the endpoint never started serving: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRegisterServicesNodeRoleServesOnlyIdentityAndNode(t *testing.T) {
	t.Parallel()

	conn := serveRegistered(t, nodeTestConfig())

	info, err := csi.NewIdentityClient(conn).GetPluginInfo(
		t.Context(),
		&csi.GetPluginInfoRequest{},
	)
	if err != nil {
		t.Fatalf("expected the identity service to answer, got: %v", err)
	}
	if info.GetName() != config.DefaultDriverName {
		t.Errorf("expected plugin name %q, got %q", config.DefaultDriverName, info.GetName())
	}

	if _, err := csi.NewNodeClient(conn).NodeGetCapabilities(
		t.Context(),
		&csi.NodeGetCapabilitiesRequest{},
	); err != nil {
		t.Fatalf("expected the node service to answer, got: %v", err)
	}

	_, err = csi.NewControllerClient(conn).ControllerGetCapabilities(
		t.Context(),
		&csi.ControllerGetCapabilitiesRequest{},
	)
	assertUnknownService(t, err)
}

func TestListenCreatesTheEndpointParentDirectory(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "nested", "sockets", "csi.sock")
	cfg := &config.Config{Endpoint: "unix://" + socketPath}

	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("expected listen to create the parent directory, got: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	if _, err := os.Stat(filepath.Dir(socketPath)); err != nil {
		t.Errorf("expected the endpoint directory to exist: %v", err)
	}
}

// TestListenBindsTheResolvedTarget requires the open listener to be the target configuration
// resolved, so binding cannot reach a network or address validation never approved.
func TestListenBindsTheResolvedTarget(t *testing.T) {
	t.Parallel()

	unixEndpoint, _ := testSocketEndpoint(t)

	// A tcp port of zero lets the kernel choose, so only the host is comparable.
	testCases := []struct {
		name        string
		endpoint    string
		wantNetwork string
		hostOnly    bool
	}{
		{"unix endpoint", unixEndpoint, "unix", false},
		{"tcp endpoint", "tcp://127.0.0.1:0", "tcp", true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{Endpoint: tc.endpoint}
			wantNetwork, wantAddress, err := cfg.Network()
			if err != nil {
				t.Fatalf("expected endpoint %q to resolve, got: %v", tc.endpoint, err)
			}
			if wantNetwork != tc.wantNetwork {
				t.Fatalf("expected network %q, got %q", tc.wantNetwork, wantNetwork)
			}

			listener, err := listen(cfg)
			if err != nil {
				t.Fatalf("expected listen to open %q, got: %v", tc.endpoint, err)
			}
			t.Cleanup(func() { _ = listener.Close() })

			bound := listener.Addr()
			if bound.Network() != wantNetwork {
				t.Errorf(
					"listener bound network %q, resolution approved %q",
					bound.Network(),
					wantNetwork,
				)
			}

			gotAddress := bound.String()
			if tc.hostOnly {
				gotAddress = hostOf(t, gotAddress)
				wantAddress = hostOf(t, wantAddress)
			}
			if gotAddress != wantAddress {
				t.Errorf(
					"listener bound address %q, resolution approved %q",
					gotAddress,
					wantAddress,
				)
			}
		})
	}
}

// hostOf returns the host of a host:port address.
func hostOf(t *testing.T, address string) string {
	t.Helper()

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("failed to split address %q: %v", address, err)
	}

	return host
}

func TestListenRefusesANonSocketEndpointPathWithoutRemovingIt(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "csi.sock")
	if err := os.WriteFile(socketPath, []byte("unrelated"), 0o600); err != nil {
		t.Fatalf("failed to create the occupying file: %v", err)
	}
	cfg := &config.Config{Endpoint: "unix://" + socketPath}

	_, err := listen(cfg)
	if err == nil {
		t.Fatal("expected listen to refuse a non-socket endpoint path")
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("expected error to mention non-socket path, got: %v", err)
	}

	content, readErr := os.ReadFile(socketPath)
	if readErr != nil || string(content) != "unrelated" {
		t.Errorf("the refused path was modified: %q, %v", content, readErr)
	}
}

func TestListenClearsAStaleSocket(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "csi.sock")

	stale, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create the stale socket: %v", err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("test setup left no stale socket: %v", err)
	}

	cfg := &config.Config{Endpoint: "unix://" + socketPath}
	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("expected listen to clear the stale socket, got: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
}

// blockingIdentity holds Probe until released so a stop can be observed mid-call.
type blockingIdentity struct {
	csi.UnimplementedIdentityServer
	entered chan struct{}
	release chan struct{}
}

func (s *blockingIdentity) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	close(s.entered)
	<-s.release
	return &csi.ProbeResponse{}, nil
}

func TestServeStopsGracefullyAndLetsAnInFlightCallFinish(t *testing.T) {
	t.Parallel()

	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	svc := &blockingIdentity{entered: make(chan struct{}), release: make(chan struct{})}
	csi.RegisterIdentityServer(server, svc)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener) }()

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}
	defer func() { _ = conn.Close() }()

	probed := make(chan error, 1)
	go func() {
		_, err := csi.NewIdentityClient(conn).Probe(t.Context(), &csi.ProbeRequest{})
		probed <- err
	}()

	select {
	case <-svc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight call never reached the handler")
	}

	cancel()

	// The stop must wait for the in-flight call, not kill it.
	select {
	case err := <-probed:
		t.Fatalf("the stop ended the in-flight call: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(svc.release)

	if err := <-probed; err != nil {
		t.Fatalf("expected the in-flight call to finish, got: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("expected a stopped server to report no failure, got: %v", err)
	}
}
