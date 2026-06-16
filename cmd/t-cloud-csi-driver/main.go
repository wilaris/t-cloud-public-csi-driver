// Command t-cloud-csi-driver is the CSI plugin entrypoint for T Cloud Public EVS.
// Controller manages cloud volumes (and holds credentials); node does local storage work.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/mount"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/version"
)

// socketDirPerm is the permission mode for a created endpoint socket directory.
const socketDirPerm = 0o750

func main() {
	logger := log.NewLoggerFromEnv(os.Stdout)

	info := version.Get()
	logger.Info(
		"Starting CSI driver",
		slog.String("version", info.Version),
		slog.String("commit", info.Commit),
		slog.String("build_date", info.BuildDate),
		slog.String("go", info.GoVersion),
		slog.String("platform", info.Platform),
	)

	err := run(logger)
	if errors.Is(err, flag.ErrHelp) || errors.Is(err, config.ErrVersionRequested) {
		return
	}
	if err != nil {
		logger.Error("Driver startup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// run builds the selected role and serves until SIGINT/SIGTERM. Config and
// dependencies are resolved before listen so a failed start leaves no socket.
func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadFromEnvAndFlags(ctx, os.Args[1:])
	if err != nil {
		return err
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(unaryLoggingInterceptor(logger)))
	if err := registerServices(ctx, server, cfg, logger); err != nil {
		return err
	}

	listener, err := listen(cfg)
	if err != nil {
		return err
	}

	logger.Info(
		"Serving CSI",
		slog.String("role", string(cfg.Role)),
		slog.String("driver", cfg.DriverName),
		slog.String("version", cfg.Version),
	)

	return serve(ctx, server, listener)
}

// registerServices registers Identity and the service for cfg.Role.
func registerServices(
	ctx context.Context,
	server *grpc.Server,
	cfg *config.Config,
	logger *slog.Logger,
) error {
	identity, err := driver.NewIdentityService(cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to construct identity service: %w", err)
	}
	csi.RegisterIdentityServer(server, identity)

	switch cfg.Role {
	case config.RoleController:
		return registerController(ctx, server, cfg, logger)
	case config.RoleNode:
		return registerNode(server, cfg, logger)
	default:
		return fmt.Errorf("unsupported driver role %q", cfg.Role)
	}
}

// registerController builds the controller path. EVS client construction
// authenticates, so bad credentials fail at startup and a signal during that
// exchange stops it.
func registerController(
	ctx context.Context,
	server *grpc.Server,
	cfg *config.Config,
	logger *slog.Logger,
) error {
	evsClient, err := evs.NewClient(ctx, evs.Config{
		AuthURL:       cfg.AuthURL,
		AccessKey:     cfg.AccessKey.Value(),
		SecretKey:     cfg.SecretKey.Value(),
		ProjectID:     cfg.ProjectID,
		RegionName:    cfg.RegionName,
		SecurityToken: cfg.SecurityToken.Value(),
	})
	if err != nil {
		return fmt.Errorf("failed to construct EVS client: %w", err)
	}

	controller, err := driver.NewControllerService(evsClient, cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to construct controller service: %w", err)
	}
	csi.RegisterControllerServer(server, controller)

	return nil
}

// registerNode builds the node-local half. No EVS client is constructed on this path.
func registerNode(server *grpc.Server, cfg *config.Config, logger *slog.Logger) error {
	node, err := driver.NewNodeService(mount.NewLinuxMounter(), cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to construct node service: %w", err)
	}
	csi.RegisterNodeServer(server, node)

	return nil
}

// listen opens the configured endpoint.
func listen(cfg *config.Config) (net.Listener, error) {
	network, address, err := cfg.Network()
	if err != nil {
		return nil, err
	}

	if network == "tcp" {
		listener, err := net.Listen(network, address)
		if err != nil {
			return nil, fmt.Errorf("failed to listen on tcp endpoint: %w", err)
		}
		return listener, nil
	}

	//nolint:gosec // endpoint path constrained at configuration ingress
	if err := os.MkdirAll(filepath.Dir(address), socketDirPerm); err != nil {
		return nil, fmt.Errorf("failed to create endpoint directory: %w", err)
	}

	if err := clearStaleSocket(address); err != nil {
		return nil, err
	}

	listener, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on unix endpoint: %w", err)
	}

	return listener, nil
}

// clearStaleSocket removes a leftover unix endpoint socket.
func clearStaleSocket(socketPath string) error {
	//nolint:gosec // endpoint path constrained at configuration ingress
	info, err := os.Stat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to inspect endpoint path: %w", err)
	}

	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("endpoint path already exists and is not a socket")
	}

	//nolint:gosec // endpoint path constrained at configuration ingress
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("failed to remove stale endpoint socket: %w", err)
	}

	return nil
}

// serve runs the gRPC server until it fails or the process is signaled, then
// GracefulStop so in-flight RPCs can finish.
func serve(ctx context.Context, server *grpc.Server, listener net.Listener) error {
	served := make(chan error, 1)
	go func() {
		served <- server.Serve(listener)
	}()

	select {
	case err := <-served:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("csi server failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		server.GracefulStop()
		return nil
	}
}
