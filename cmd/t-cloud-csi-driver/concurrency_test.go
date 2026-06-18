package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/driver"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/mount"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockingProbeIdentity holds Probe calls until released to test in-flight RPC handling.
type blockingProbeIdentity struct {
	csi.UnimplementedIdentityServer
	entered chan struct{}
	release chan struct{}
}

func newBlockingProbeIdentity() *blockingProbeIdentity {
	return &blockingProbeIdentity{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *blockingProbeIdentity) Probe(
	_ context.Context,
	_ *csi.ProbeRequest,
) (*csi.ProbeResponse, error) {
	s.entered <- struct{}{}
	<-s.release
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}

func (s *blockingProbeIdentity) GetPluginInfo(
	_ context.Context,
	_ *csi.GetPluginInfoRequest,
) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          config.DefaultDriverName,
		VendorVersion: config.DriverVersion,
	}, nil
}

func (s *blockingProbeIdentity) GetPluginCapabilities(
	_ context.Context,
	_ *csi.GetPluginCapabilitiesRequest,
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

// countHandler is a thread-safe slog handler recording record counts and levels.
type countHandler struct {
	mu     sync.Mutex
	counts map[slog.Level]int
	total  int
}

func newCountHandler() *countHandler {
	return &countHandler{
		counts: make(map[slog.Level]int),
	}
}

func (h *countHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *countHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[r.Level]++
	h.total++
	return nil
}

func (h *countHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *countHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *countHandler) getCount(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[level]
}

func (h *countHandler) getTotal() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

func TestUnaryLoggingInterceptorConcurrency_HighThroughput(t *testing.T) {
	t.Parallel()

	handler := newCountHandler()
	logger := slog.New(handler)
	interceptor := unaryLoggingInterceptor(logger)

	const workers = 30
	const iterationsPerWorker = 50
	const totalExpected = workers * iterationsPerWorker

	var wg sync.WaitGroup
	start := make(chan struct{})

	testCases := []struct {
		code      codes.Code
		wantLevel slog.Level
		hasError  bool
	}{
		{codes.OK, slog.LevelDebug, false},
		{codes.InvalidArgument, slog.LevelWarn, true},
		{codes.NotFound, slog.LevelWarn, true},
		{codes.AlreadyExists, slog.LevelWarn, true},
		{codes.FailedPrecondition, slog.LevelWarn, true},
		{codes.Aborted, slog.LevelWarn, true},
		{codes.Canceled, slog.LevelWarn, true},
		{codes.Internal, slog.LevelError, true},
		{codes.Unavailable, slog.LevelError, true},
		{codes.Unauthenticated, slog.LevelError, true},
	}

	for w := 0; w < workers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for i := 0; i < iterationsPerWorker; i++ {
				tc := testCases[(workerID+i)%len(testCases)]
				method := fmt.Sprintf("/csi.v1.TestService/Method%d", (workerID+i)%5)

				var handlerErr error
				if tc.hasError {
					handlerErr = status.Error(tc.code, fmt.Sprintf("failure for code %s", tc.code))
				}

				resp, err := interceptor(
					context.Background(),
					fmt.Sprintf("request-%d-%d", workerID, i),
					&grpc.UnaryServerInfo{FullMethod: method},
					func(ctx context.Context, _ any) (any, error) {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						return fmt.Sprintf("response-%d-%d", workerID, i), handlerErr
					},
				)

				if tc.hasError {
					if err == nil {
						t.Errorf(
							"worker %d iter %d expected error for code %s, got nil",
							workerID,
							i,
							tc.code,
						)
						return
					}
					if status.Code(err) != tc.code {
						t.Errorf(
							"worker %d iter %d got code %s, want %s",
							workerID,
							i,
							status.Code(err),
							tc.code,
						)
						return
					}
				} else {
					if err != nil {
						t.Errorf("worker %d iter %d unexpected error: %v", workerID, i, err)
						return
					}
					if resp != fmt.Sprintf("response-%d-%d", workerID, i) {
						t.Errorf("worker %d iter %d unexpected resp: %v", workerID, i, resp)
						return
					}
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	if total := handler.getTotal(); total != totalExpected {
		t.Errorf("total logged records = %d, want %d", total, totalExpected)
	}

	debugCount := handler.getCount(slog.LevelDebug)
	warnCount := handler.getCount(slog.LevelWarn)
	errorCount := handler.getCount(slog.LevelError)

	if debugCount == 0 {
		t.Errorf("expected debug records for OK status, got 0")
	}
	if warnCount == 0 {
		t.Errorf("expected warn records for retryable status codes, got 0")
	}
	if errorCount == 0 {
		t.Errorf("expected error records for terminal status codes, got 0")
	}
	if debugCount+warnCount+errorCount != totalExpected {
		t.Errorf(
			"sum of levels (%d + %d + %d = %d) != total expected %d",
			debugCount,
			warnCount,
			errorCount,
			debugCount+warnCount+errorCount,
			totalExpected,
		)
	}
}

func TestUnaryLoggingInterceptorConcurrency_CredentialRedactionStress(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	logger := log.NewLogger(&buf, slog.LevelInfo)
	interceptor := unaryLoggingInterceptor(logger)

	const workers = 25
	const iterationsPerWorker = 30

	secrets := []string{
		"superSecretKey998877",
		"anotherVerySecretToken112233",
		"accessKeySecretValue445566",
		"bearerTokenString778899",
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < workers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for i := 0; i < iterationsPerWorker; i++ {
				secret := secrets[(workerID+i)%len(secrets)]
				handlerErr := status.Error(
					codes.Internal,
					fmt.Sprintf("call failed: OS_SECRET_KEY=%s, OS_ACCESS_KEY=%s", secret, secret),
				)

				_, err := interceptor(
					context.Background(),
					struct{}{},
					&grpc.UnaryServerInfo{FullMethod: "/csi.v1.Controller/CreateVolume"},
					func(context.Context, any) (any, error) {
						return nil, handlerErr
					},
				)
				if err == nil {
					t.Errorf("worker %d iter %d expected error", workerID, i)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	loggedOutput := buf.String()
	for _, secret := range secrets {
		if strings.Contains(loggedOutput, secret) {
			t.Errorf("secret %q leaked in log output under concurrent execution", secret)
		}
	}
	if !strings.Contains(loggedOutput, "***") {
		t.Errorf("expected *** redaction markers in log output")
	}
}

func TestServeConcurrency_GracefulShutdownMultipleInFlight(t *testing.T) {
	t.Parallel()

	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	svc := newBlockingProbeIdentity()
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

	const inFlightWorkers = 15
	var inFlightWg sync.WaitGroup
	results := make([]error, inFlightWorkers)
	enteredCount := int32(0)

	for i := 0; i < inFlightWorkers; i++ {
		workerID := i
		inFlightWg.Add(1)
		go func() {
			defer inFlightWg.Done()
			client := csi.NewIdentityClient(conn)
			_, callErr := client.Probe(t.Context(), &csi.ProbeRequest{})
			results[workerID] = callErr
		}()
	}

	// Drain entered signals until all workers are blocked inside the handler.
	for i := 0; i < inFlightWorkers; i++ {
		select {
		case <-svc.entered:
			atomic.AddInt32(&enteredCount, 1)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for worker %d to enter handler", i)
		}
	}

	if got := atomic.LoadInt32(&enteredCount); got != inFlightWorkers {
		t.Fatalf("entered workers = %d, want %d", got, inFlightWorkers)
	}

	// Trigger graceful shutdown while all RPCs are actively executing.
	cancel()

	// Verify serve has not finished yet because workers are still in-flight.
	select {
	case err := <-served:
		t.Fatalf("serve returned prematurely before in-flight RPCs completed: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// Release all in-flight workers.
	close(svc.release)
	inFlightWg.Wait()

	// All in-flight calls must finish without error.
	for i := 0; i < inFlightWorkers; i++ {
		if results[i] != nil {
			t.Errorf("worker %d Probe() unexpected error: %v", i, results[i])
		}
	}

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve() returned unexpected error on graceful stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not unblock after all in-flight calls finished")
	}
}

func TestServeConcurrency_BurstTrafficAndClientChurn(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "burst-csi.sock")
	cfg := &config.Config{
		Role:             config.RoleNode,
		Endpoint:         "unix://" + socketPath,
		NodeID:           "test-node-uuid-burst",
		AvailabilityZone: "eu-de-01",
		DriverName:       config.DefaultDriverName,
		Version:          config.DriverVersion,
	}

	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen() error: %v", err)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(unaryLoggingInterceptor(discardLogger())))
	identity, err := driver.NewIdentityService(cfg, discardLogger())
	if err != nil {
		_ = listener.Close()
		t.Fatalf("NewIdentityService() error: %v", err)
	}
	csi.RegisterIdentityServer(server, identity)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener) }()

	const clientWorkers = 10
	const iterationsPerWorker = 10

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < clientWorkers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for i := 0; i < iterationsPerWorker; i++ {
				conn, dialErr := grpc.NewClient(
					"unix://"+socketPath,
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if dialErr != nil {
					t.Errorf("worker %d iter %d dial error: %v", workerID, i, dialErr)
					return
				}

				client := csi.NewIdentityClient(conn)

				probeResp, probeErr := client.Probe(context.Background(), &csi.ProbeRequest{})
				if probeErr != nil {
					_ = conn.Close()
					t.Errorf("worker %d iter %d Probe() error: %v", workerID, i, probeErr)
					return
				}
				if !probeResp.GetReady().GetValue() {
					_ = conn.Close()
					t.Errorf("worker %d iter %d Probe() ready = false, want true", workerID, i)
					return
				}

				infoResp, infoErr := client.GetPluginInfo(
					context.Background(),
					&csi.GetPluginInfoRequest{},
				)
				if infoErr != nil {
					_ = conn.Close()
					t.Errorf("worker %d iter %d GetPluginInfo() error: %v", workerID, i, infoErr)
					return
				}
				if infoResp.GetName() != config.DefaultDriverName {
					_ = conn.Close()
					t.Errorf(
						"worker %d iter %d plugin name = %q, want %q",
						workerID,
						i,
						infoResp.GetName(),
						config.DefaultDriverName,
					)
					return
				}

				capsResp, capsErr := client.GetPluginCapabilities(
					context.Background(),
					&csi.GetPluginCapabilitiesRequest{},
				)
				if capsErr != nil {
					_ = conn.Close()
					t.Errorf(
						"worker %d iter %d GetPluginCapabilities() error: %v",
						workerID,
						i,
						capsErr,
					)
					return
				}
				if len(capsResp.GetCapabilities()) == 0 {
					_ = conn.Close()
					t.Errorf("worker %d iter %d returned empty capabilities", workerID, i)
					return
				}

				if closeErr := conn.Close(); closeErr != nil {
					t.Errorf("worker %d iter %d conn.Close() error: %v", workerID, i, closeErr)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve() failed after burst client churn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() failed to stop after burst client churn")
	}
}

func TestServeConcurrency_ShutdownWithIncomingTraffic(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "shutdown-traffic-csi.sock")
	cfg := &config.Config{
		Role:             config.RoleNode,
		Endpoint:         "unix://" + socketPath,
		NodeID:           "test-node-uuid-shutdown",
		AvailabilityZone: "eu-de-01",
		DriverName:       config.DefaultDriverName,
		Version:          config.DriverVersion,
	}

	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen() error: %v", err)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(unaryLoggingInterceptor(discardLogger())))
	identity, err := driver.NewIdentityService(cfg, discardLogger())
	if err != nil {
		_ = listener.Close()
		t.Fatalf("NewIdentityService() error: %v", err)
	}
	csi.RegisterIdentityServer(server, identity)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener) }()

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial unix socket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	const trafficWorkers = 10
	stopTraffic := make(chan struct{})
	var wg sync.WaitGroup

	for w := 0; w < trafficWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := csi.NewIdentityClient(conn)
			for {
				select {
				case <-stopTraffic:
					return
				default:
					ctxTimeout, cancelTimeout := context.WithTimeout(
						t.Context(),
						50*time.Millisecond,
					)
					_, _ = client.Probe(ctxTimeout, &csi.ProbeRequest{})
					cancelTimeout()
					time.Sleep(2 * time.Millisecond)
				}
			}
		}()
	}

	// Allow traffic to flow for a brief window.
	time.Sleep(50 * time.Millisecond)

	// Cancel server context while requests are actively in-flight and arriving.
	cancel()

	// Stop client traffic loop.
	close(stopTraffic)
	wg.Wait()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve() returned error on shutdown with incoming traffic: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not stop within deadline under concurrent incoming traffic")
	}
}

func TestListenAndServeConcurrency_UnixSocketLifecycle(t *testing.T) {
	t.Parallel()

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "concurrent-csi.sock")
	cfg := &config.Config{
		Role:             config.RoleNode,
		Endpoint:         "unix://" + socketPath,
		NodeID:           "test-node-uuid-1234",
		AvailabilityZone: "eu-de-01",
		DriverName:       config.DefaultDriverName,
		Version:          config.DriverVersion,
	}

	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen(%q) error: %v", cfg.Endpoint, err)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(unaryLoggingInterceptor(discardLogger())))
	identity, err := driver.NewIdentityService(cfg, discardLogger())
	if err != nil {
		_ = listener.Close()
		t.Fatalf("NewIdentityService() error: %v", err)
	}
	csi.RegisterIdentityServer(server, identity)

	node, err := driver.NewNodeService(mount.NewLinuxMounter(), cfg, discardLogger())
	if err != nil {
		_ = listener.Close()
		t.Fatalf("NewNodeService() error: %v", err)
	}
	csi.RegisterNodeServer(server, node)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener) }()

	const workers = 15
	const iterations = 20

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < workers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			conn, dialErr := grpc.NewClient(
				"unix://"+socketPath,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if dialErr != nil {
				t.Errorf("worker %d unix socket dial error: %v", workerID, dialErr)
				return
			}
			defer func() { _ = conn.Close() }()

			identityClient := csi.NewIdentityClient(conn)
			nodeClient := csi.NewNodeClient(conn)

			for i := 0; i < iterations; i++ {
				probeResp, probeErr := identityClient.Probe(
					context.Background(),
					&csi.ProbeRequest{},
				)
				if probeErr != nil {
					t.Errorf("worker %d iter %d Probe error: %v", workerID, i, probeErr)
					return
				}
				if !probeResp.GetReady().GetValue() {
					t.Errorf("worker %d iter %d Probe not ready", workerID, i)
					return
				}

				nodeCaps, nodeCapsErr := nodeClient.NodeGetCapabilities(
					context.Background(),
					&csi.NodeGetCapabilitiesRequest{},
				)
				if nodeCapsErr != nil {
					t.Errorf(
						"worker %d iter %d NodeGetCapabilities error: %v",
						workerID,
						i,
						nodeCapsErr,
					)
					return
				}
				if len(nodeCaps.GetCapabilities()) == 0 {
					t.Errorf("worker %d iter %d empty node capabilities", workerID, i)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve() failed on unix socket shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not stop cleanly on unix socket")
	}

	// Verify clearStaleSocket handles leftover socket file properly.
	if err := clearStaleSocket(socketPath); err != nil {
		t.Errorf("clearStaleSocket failed on stopped socket: %v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file was not removed by clearStaleSocket: %s", socketPath)
	}
}

func TestClearStaleSocketConcurrency(t *testing.T) {
	t.Parallel()

	const workers = 20
	const iterations = 15

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < workers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for i := 0; i < iterations; i++ {
				// Test with non-existent socket.
				nonExistentPath := filepath.Join(
					t.TempDir(),
					fmt.Sprintf("absent-%d-%d.sock", workerID, i),
				)
				if err := clearStaleSocket(nonExistentPath); err != nil {
					t.Errorf("clearStaleSocket unexpected error on absent path: %v", err)
					return
				}

				// Test with real stale socket.
				stalePath := filepath.Join(
					t.TempDir(),
					fmt.Sprintf("stale-%d-%d.sock", workerID, i),
				)
				l, listenErr := net.Listen("unix", stalePath)
				if listenErr != nil {
					t.Errorf("failed to create unix socket fixture: %v", listenErr)
					return
				}
				l.(*net.UnixListener).SetUnlinkOnClose(false)
				_ = l.Close()

				if err := clearStaleSocket(stalePath); err != nil {
					t.Errorf("clearStaleSocket failed to remove stale socket: %v", err)
					return
				}

				// Test with non-socket file (must be refused without deletion).
				filePath := filepath.Join(
					t.TempDir(),
					fmt.Sprintf("regular-%d-%d.txt", workerID, i),
				)
				if err := os.WriteFile(filePath, []byte("preserve-content"), 0o600); err != nil {
					t.Errorf("failed to write regular file fixture: %v", err)
					return
				}
				if err := clearStaleSocket(filePath); err == nil {
					t.Errorf("clearStaleSocket should have refused regular file: %s", filePath)
					return
				}
				if _, statErr := os.Stat(filePath); statErr != nil {
					t.Errorf("clearStaleSocket deleted regular file: %v", statErr)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}

func TestUnaryLoggingInterceptorConcurrency_MixedErrorsAndContextCancellation(t *testing.T) {
	t.Parallel()

	handler := newCountHandler()
	logger := slog.New(handler)
	interceptor := unaryLoggingInterceptor(logger)

	const workers = 25
	const iterations = 30

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < workers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for i := 0; i < iterations; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				// Cancel every odd iteration.
				if (workerID+i)%2 == 1 {
					cancel()
				} else {
					defer cancel()
				}

				var returnErr error
				if ctx.Err() != nil {
					returnErr = status.Error(codes.Canceled, "request context canceled")
				} else if i%3 == 0 {
					returnErr = status.Error(codes.DeadlineExceeded, "context deadline exceeded")
				} else if i%3 == 1 {
					returnErr = status.Error(codes.Internal, "internal database failure")
				}

				_, err := interceptor(
					ctx,
					struct{}{},
					&grpc.UnaryServerInfo{FullMethod: "/csi.v1.Identity/Probe"},
					func(callCtx context.Context, _ any) (any, error) {
						if err := callCtx.Err(); err != nil {
							return nil, status.Error(codes.Canceled, err.Error())
						}
						return struct{}{}, returnErr
					},
				)

				if returnErr != nil || ctx.Err() != nil {
					if err == nil {
						t.Errorf("worker %d iter %d expected non-nil error", workerID, i)
						return
					}
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	if total := handler.getTotal(); total != workers*iterations {
		t.Errorf("total logged calls = %d, want %d", total, workers*iterations)
	}
}

func TestRegisterServicesNodeRoleConcurrency(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "node-role-concurrency.sock")
	cfg := &config.Config{
		Role:             config.RoleNode,
		Endpoint:         "unix://" + socketPath,
		NodeID:           "test-node-uuid-role-test",
		AvailabilityZone: "eu-de-01",
		DriverName:       config.DefaultDriverName,
		Version:          config.DriverVersion,
	}

	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(unaryLoggingInterceptor(discardLogger())))
	if err := registerServices(t.Context(), server, cfg, discardLogger()); err != nil {
		_ = listener.Close()
		t.Fatalf("registerServices error: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener) }()

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	const workers = 20
	const iterations = 25

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < workers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			identityClient := csi.NewIdentityClient(conn)
			nodeClient := csi.NewNodeClient(conn)

			for i := 0; i < iterations; i++ {
				// Probe
				probeResp, probeErr := identityClient.Probe(
					context.Background(),
					&csi.ProbeRequest{},
				)
				if probeErr != nil {
					t.Errorf("worker %d iter %d Probe error: %v", workerID, i, probeErr)
					return
				}
				if !probeResp.GetReady().GetValue() {
					t.Errorf("worker %d iter %d Probe not ready", workerID, i)
					return
				}

				// NodeGetInfo
				infoResp, infoErr := nodeClient.NodeGetInfo(
					context.Background(),
					&csi.NodeGetInfoRequest{},
				)
				if infoErr != nil {
					t.Errorf("worker %d iter %d NodeGetInfo error: %v", workerID, i, infoErr)
					return
				}
				if infoResp.GetNodeId() != cfg.NodeID {
					t.Errorf(
						"worker %d iter %d NodeId = %q, want %q",
						workerID,
						i,
						infoResp.GetNodeId(),
						cfg.NodeID,
					)
					return
				}
				if infoResp.GetAccessibleTopology().GetSegments()[driver.TopologyZoneKey] != cfg.AvailabilityZone {
					t.Errorf(
						"worker %d iter %d zone = %q, want %q",
						workerID,
						i,
						infoResp.GetAccessibleTopology().GetSegments()[driver.TopologyZoneKey],
						cfg.AvailabilityZone,
					)
					return
				}

				// NodeGetCapabilities
				capsResp, capsErr := nodeClient.NodeGetCapabilities(
					context.Background(),
					&csi.NodeGetCapabilitiesRequest{},
				)
				if capsErr != nil {
					t.Errorf(
						"worker %d iter %d NodeGetCapabilities error: %v",
						workerID,
						i,
						capsErr,
					)
					return
				}
				if len(capsResp.GetCapabilities()) == 0 {
					t.Errorf("worker %d iter %d empty capabilities", workerID, i)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop cleanly")
	}
}
