package driver_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
)

const bufSize = 1024 * 1024

// discardLogger is a no-op logger for tests that do not assert on log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// captureLogger returns the production logger writing into a buffer (includes redaction).
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer

	return log.NewLogger(&buf, slog.LevelDebug), &buf
}

// serveCSI starts an in-process gRPC server with register and returns a client conn.
// Conn, server and listener are cleaned up with t.Cleanup.
func serveCSI(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()

	ln := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	register(server)

	go func() {
		if err := server.Serve(ln); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("gRPC server error: %v", err)
		}
	}()

	bufDialer := func(context.Context, string) (net.Conn, error) {
		return ln.Dial()
	}

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(bufDialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		server.GracefulStop()
		_ = ln.Close()
	})

	return conn
}

// newIdentityClient serves svc as the CSI Identity service and returns a client for it.
func newIdentityClient(t *testing.T, svc csi.IdentityServer) csi.IdentityClient {
	t.Helper()

	return csi.NewIdentityClient(serveCSI(t, func(server *grpc.Server) {
		csi.RegisterIdentityServer(server, svc)
	}))
}

// newControllerClient serves svc as the CSI Controller service and returns a client for it.
func newControllerClient(t *testing.T, svc csi.ControllerServer) csi.ControllerClient {
	t.Helper()

	return csi.NewControllerClient(serveCSI(t, func(server *grpc.Server) {
		csi.RegisterControllerServer(server, svc)
	}))
}

// newNodeClient serves svc as the CSI Node service and returns a client for it.
func newNodeClient(t *testing.T, svc csi.NodeServer) csi.NodeClient {
	t.Helper()

	return csi.NewNodeClient(serveCSI(t, func(server *grpc.Server) {
		csi.RegisterNodeServer(server, svc)
	}))
}

// validTestConfig returns a configuration every driver service accepts.
func validTestConfig() *config.Config {
	return &config.Config{
		Endpoint:         "unix:///tmp/csi.sock",
		NodeID:           "12345678-1234-1234-1234-123456789012",
		DriverName:       "evs.csi.t-cloud.wilaris.dev",
		Version:          "v0.1.0",
		AvailabilityZone: "eu-de-01",
		AuthURL:          "https://iam.example.com/v3",
		AccessKey:        "test-ak",
		SecretKey:        "test-sk",
		ProjectID:        "test-project-id",
		RegionName:       "eu-de",
	}
}

// accessModeCapability describes a volume by access mode alone, leaving the access type unset.
func accessModeCapability(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: mode,
		},
	}
}

// mountCapability describes a mounted filesystem volume of the given type for the single-node
// writer access mode.
func mountCapability(fsType string) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fsType},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

// blockCapability describes a raw block volume for the single-node writer access mode.
func blockCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{
			Block: &csi.VolumeCapability_BlockVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}
