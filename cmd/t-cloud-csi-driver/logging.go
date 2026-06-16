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
