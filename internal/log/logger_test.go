package log_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
)

type sampleAuthConfig struct {
	OS_ACCESS_KEY     string
	OS_SECRET_KEY     string
	OS_SECURITY_TOKEN string
}

func (s sampleAuthConfig) String() string {
	return fmt.Sprintf("sampleAuthConfig{AK:%s, SK:%s}", s.OS_ACCESS_KEY, s.OS_SECRET_KEY)
}

// sampleAuthError is an error whose message embeds a secret (value receiver; typed nil panics if
// called).
type sampleAuthError struct {
	OS_SECRET_KEY string
}

func (s sampleAuthError) Error() string {
	return fmt.Sprintf("sampleAuthError{SK:%s}", s.OS_SECRET_KEY)
}

// deferredAuthConfig is a slog.LogValuer that only exposes credentials when resolved.
type deferredAuthConfig struct {
	accessKey string
	secretKey string
}

func (d deferredAuthConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("OS_ACCESS_KEY", d.accessKey),
		slog.String("OS_SECRET_KEY", d.secretKey),
	)
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"", slog.LevelInfo},
		{"invalid", slog.LevelInfo},
		{"12345", slog.LevelInfo},
	}

	for _, tt := range tests {
		actual := log.ParseLevel(tt.input)
		if actual != tt.expected {
			t.Errorf("ParseLevel(%q) = %v; want %v", tt.input, actual, tt.expected)
		}
	}
}

func TestSanitizingLogger_RedactsMessageAndAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.NewLogger(&buf, slog.LevelDebug)

	ak := "AKIAIOSFODNN7EXAMPLE"
	sk := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	token := "securityTokenValue123"
	auth := "Bearer secretAuthTokenValue"

	logger.Info(
		fmt.Sprintf("Connecting with OS_ACCESS_KEY=%s and OS_SECRET_KEY=%s", ak, sk),
		slog.String("OS_ACCESS_KEY", ak),
		slog.String("OS_SECRET_KEY", sk),
		slog.String("OS_SECURITY_TOKEN", token),
		slog.String("Authorization", auth),
	)

	out := buf.String()

	if strings.Contains(out, ak) {
		t.Errorf("Log output contains unmasked OS_ACCESS_KEY value %q: %s", ak, out)
	}
	if strings.Contains(out, sk) {
		t.Errorf("Log output contains unmasked OS_SECRET_KEY value %q: %s", sk, out)
	}
	if strings.Contains(out, token) {
		t.Errorf("Log output contains unmasked OS_SECURITY_TOKEN value %q: %s", token, out)
	}
	if strings.Contains(out, "secretAuthTokenValue") {
		t.Errorf("Log output contains unmasked Authorization token value: %s", out)
	}

	if !strings.Contains(out, "***") {
		t.Errorf("Log output missing expected *** mask: %s", out)
	}
}

func TestSanitizingLogger_RedactsErrors(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.NewLogger(&buf, slog.LevelInfo)

	sk := "superSecretKey123"
	err := fmt.Errorf(
		"failed request with OS_SECRET_KEY=%s and Authorization: Bearer bearerToken456",
		sk,
	)

	logger.Error("operation failed", slog.Any("error", err))

	out := buf.String()
	if strings.Contains(out, sk) {
		t.Errorf("Log output contains unmasked secret in error: %s", out)
	}
	if strings.Contains(out, "bearerToken456") {
		t.Errorf("Log output contains unmasked bearer token in error: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("Log output missing *** mask for error: %s", out)
	}
}

func TestSanitizingLogger_RedactsStringifiedStructs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.NewLogger(&buf, slog.LevelInfo)

	cfg := sampleAuthConfig{
		OS_ACCESS_KEY:     "myAK123",
		OS_SECRET_KEY:     "mySK456",
		OS_SECURITY_TOKEN: "myToken789",
	}

	logger.Info("initializing client", slog.Any("config", cfg))

	out := buf.String()
	if strings.Contains(out, "myAK123") {
		t.Errorf("Log output contains unmasked AK from struct: %s", out)
	}
	if strings.Contains(out, "mySK456") {
		t.Errorf("Log output contains unmasked SK from struct: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("Log output missing *** mask for struct fields: %s", out)
	}
}

func TestSanitizingLogger_RedactsDeferredValues(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.NewLogger(&buf, slog.LevelInfo)

	logger.Info("initializing client", slog.Any("config", deferredAuthConfig{
		accessKey: "deferredAK123",
		secretKey: "deferredSK456",
	}))

	out := buf.String()
	if strings.Contains(out, "deferredAK123") {
		t.Errorf("Log output contains unmasked AK from a deferred value: %s", out)
	}
	if strings.Contains(out, "deferredSK456") {
		t.Errorf("Log output contains unmasked SK from a deferred value: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("Log output missing *** mask for a deferred value: %s", out)
	}
}

func TestSanitizingLogger_TypedNilValueDoesNotPanic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value any
	}{
		{name: "a typed nil satisfying fmt.Stringer", value: (*sampleAuthConfig)(nil)},
		{name: "a typed nil satisfying error", value: (*sampleAuthError)(nil)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.NewLogger(&buf, slog.LevelInfo)

			logger.Info("initializing client", slog.Any("config", tc.value))

			if out := buf.String(); !strings.Contains(out, "initializing client") {
				t.Errorf("Log output missing the record: %s", out)
			}
		})
	}
}

func TestSanitizingLogger_WithAttrsAndGroup(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.NewLogger(&buf, slog.LevelInfo).
		With("OS_SECRET_KEY", "secretValueFromWith").
		WithGroup("credentials")

	logger.Info("grouped log", slog.String("authorization", "Bearer groupedToken"))

	out := buf.String()
	if strings.Contains(out, "secretValueFromWith") {
		t.Errorf("Log output contains unmasked secret from With: %s", out)
	}
	if strings.Contains(out, "groupedToken") {
		t.Errorf("Log output contains unmasked token from group attribute: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("Log output missing *** mask: %s", out)
	}
}

func TestNewLoggerFromEnv(t *testing.T) {
	// Not parallel due to environment mutation
	t.Setenv("LOG_LEVEL", "warn")

	var buf bytes.Buffer
	logger := log.NewLoggerFromEnv(&buf)

	logger.Info("this info log should be filtered out")
	if buf.Len() > 0 {
		t.Errorf("Expected info log to be filtered at WARN level, got: %s", buf.String())
	}

	logger.Warn("this warn log should appear")
	if !strings.Contains(buf.String(), "this warn log should appear") {
		t.Errorf("Expected warn log to appear, got: %s", buf.String())
	}
}

func TestNewLoggerFromEnv_InvalidLevelDoesNotPanic(t *testing.T) {
	t.Setenv("LOG_LEVEL", "invalid_garbage_level")

	var buf bytes.Buffer
	logger := log.NewLoggerFromEnv(&buf)

	logger.Info("info log with default level")
	if !strings.Contains(buf.String(), "info log with default level") {
		t.Errorf("Expected default INFO level log to appear, got: %s", buf.String())
	}
}

func TestRedactString_EdgeCases(t *testing.T) {
	t.Parallel()

	if log.RedactString("") != "" {
		t.Errorf("RedactString(\"\") should return empty string")
	}

	plain := "ordinary log message without credentials"
	if log.RedactString(plain) != plain {
		t.Errorf("RedactString modified plain string unexpectedly: %s", log.RedactString(plain))
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty value does not hide a later occurrence", "SK=,SK=x", "SK=,SK=***"},
		{
			"newline terminates the value",
			"OS_SECRET_KEY=abc\nnext=line",
			"OS_SECRET_KEY=***\nnext=line",
		},
		{
			"a key ending a longer word leaves its value intact",
			"mount failed on DISK: /dev/sda1",
			"mount failed on DISK: /dev/sda1",
		},
		{"SK at the end of TASK is not the key", "TASK: reconcile", "TASK: reconcile"},
		{"AK at the end of BREAK is not the key", "BREAK: now", "BREAK: now"},
		{"a bare key masks its value", "AK=abc", "AK=***"},
		{"a key opening the text masks", "OS_SECRET_KEY=abc", "OS_SECRET_KEY=***"},
		{"an underscore-prefixed name masks", "MY_OS_SECRET_KEY=abc", "MY_OS_SECRET_KEY=***"},
		{"a dash-prefixed name masks", "X-OS_SECRET_KEY=abc", "X-OS_SECRET_KEY=***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := log.RedactString(tc.input); got != tc.want {
				t.Errorf("RedactString(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRedactAttr_UnrelatedError(t *testing.T) {
	t.Parallel()

	err := errors.New("ordinary disk I/O failure")
	attr := log.RedactAttr(slog.Any("error", err))

	if attr.Value.String() != "ordinary disk I/O failure" {
		t.Errorf("Unrelated error modified unexpectedly: %v", attr.Value)
	}
}
