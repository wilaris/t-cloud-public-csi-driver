package main

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// levelForCode maps a served RPC's status code to a log level.
//
// Success is Debug: sidecars and kubelet already log happy paths, and Probe traffic would
// dominate. Retryable CSI codes (e.g. volume still attached elsewhere) are Warn so real
// failures stay visible at Error.
func levelForCode(code codes.Code) slog.Level {
	switch code {
	case codes.OK:
		return slog.LevelDebug
	case codes.InvalidArgument,
		codes.NotFound,
		codes.AlreadyExists,
		codes.FailedPrecondition,
		codes.Aborted,
		codes.Canceled:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

// unaryLoggingInterceptor logs one line per served RPC.
//
// Only method, duration and status code are logged. The request is never read: volume
// names, target paths and publish context are caller-controlled. On failure the returned
// error is logged after the handler's own sanitization; the logger also redacts credentials.
func unaryLoggingInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		started := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(started)
		code := status.Code(err)

		attrs := []slog.Attr{
			slog.String("method", info.FullMethod),
			slog.String("code", code.String()),
			slog.Int64("duration_ms", elapsed.Milliseconds()),
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
		}

		logger.LogAttrs(ctx, levelForCode(code), "Served RPC", attrs...)
		return resp, err
	}
}
