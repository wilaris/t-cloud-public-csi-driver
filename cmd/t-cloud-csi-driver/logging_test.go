package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
)

const testMethod = "/csi.v1.Node/NodeStageVolume"

// callThroughInterceptor runs one RPC through the interceptor and returns the log output.
func callThroughInterceptor(t *testing.T, level slog.Level, handlerErr error) (string, error) {
	t.Helper()

	var buf bytes.Buffer
	interceptor := unaryLoggingInterceptor(log.NewLogger(&buf, level))

	_, err := interceptor(
		t.Context(),
		struct{}{},
		&grpc.UnaryServerInfo{FullMethod: testMethod},
		func(context.Context, any) (any, error) { return struct{}{}, handlerErr },
	)

	return buf.String(), err
}

func TestUnaryLoggingInterceptorRecordsEveryCall(t *testing.T) {
	t.Parallel()

	out, err := callThroughInterceptor(t, slog.LevelDebug, nil)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !strings.Contains(out, testMethod) {
		t.Errorf("missing method in log: %s", out)
	}
	if !strings.Contains(out, codes.OK.String()) {
		t.Errorf("missing status code in log: %s", out)
	}
	if !strings.Contains(out, "duration_ms") {
		t.Errorf("missing duration_ms in log: %s", out)
	}
}

func TestUnaryLoggingInterceptorKeepsASuccessBelowTheDefaultLevel(t *testing.T) {
	t.Parallel()

	// Sidecars already log success; driver success at Info would only add noise.
	out, err := callThroughInterceptor(t, slog.LevelInfo, nil)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if out != "" {
		t.Errorf("success logged at Info: %s", out)
	}
}

func TestUnaryLoggingInterceptorLevelsAFailureByItsCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		code  codes.Code
		level string
	}{
		{"busy volume (retryable)", codes.FailedPrecondition, `"level":"WARN"`},
		{"invalid argument", codes.InvalidArgument, `"level":"WARN"`},
		{"not found", codes.NotFound, `"level":"WARN"`},
		{"internal error", codes.Internal, `"level":"ERROR"`},
		{"unavailable", codes.Unavailable, `"level":"ERROR"`},
		{"unauthenticated", codes.Unauthenticated, `"level":"ERROR"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := callThroughInterceptor(t, slog.LevelDebug, status.Error(tc.code, "failed"))
			if err == nil {
				t.Fatal("expected handler error")
			}
			if !strings.Contains(out, tc.level) {
				t.Errorf("%s was not recorded at %s: %s", tc.code, tc.level, out)
			}
		})
	}
}

func TestUnaryLoggingInterceptorRecordsAFailedCall(t *testing.T) {
	t.Parallel()

	handlerErr := status.Error(codes.FailedPrecondition, "volume is still attached elsewhere")

	out, err := callThroughInterceptor(t, slog.LevelInfo, handlerErr)
	if err == nil {
		t.Fatal("expected handler error")
	}
	if !strings.Contains(out, codes.FailedPrecondition.String()) {
		t.Errorf("missing status code in log: %s", out)
	}
	if !strings.Contains(out, "volume is still attached elsewhere") {
		t.Errorf("missing error message in log: %s", out)
	}
}

func TestUnaryLoggingInterceptorMasksACredentialInAnError(t *testing.T) {
	t.Parallel()

	const secret = "leakedSecretValue123"
	handlerErr := status.Error(
		codes.Internal,
		fmt.Sprintf("cloud call failed with OS_SECRET_KEY=%s", secret),
	)

	out, _ := callThroughInterceptor(t, slog.LevelInfo, handlerErr)
	if strings.Contains(out, secret) {
		t.Errorf("secret leaked in log: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("expected *** redaction in log: %s", out)
	}
}

func TestUnaryLoggingInterceptorRecordsNoRequestContent(t *testing.T) {
	t.Parallel()

	// req is caller-controlled; the interceptor must not log it.
	const volumeName = "pvc-6f1c9d2e-caller-chosen-name"

	var buf bytes.Buffer
	interceptor := unaryLoggingInterceptor(log.NewLogger(&buf, slog.LevelDebug))

	_, _ = interceptor(
		t.Context(),
		map[string]string{"volume_name": volumeName, "target_path": "/var/lib/kubelet/pods/x"},
		&grpc.UnaryServerInfo{FullMethod: testMethod},
		func(context.Context, any) (any, error) { return struct{}{}, nil },
	)

	out := buf.String()
	if out == "" {
		t.Fatal("expected a record at debug level")
	}
	if strings.Contains(out, volumeName) {
		t.Errorf("volume name leaked in log: %s", out)
	}
	if strings.Contains(out, "/var/lib/kubelet/pods/x") {
		t.Errorf("target path leaked in log: %s", out)
	}
}
