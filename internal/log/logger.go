package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

var (
	sensitiveKeys = []string{
		"OS_ACCESS_KEY",
		"OS_SECRET_KEY",
		"OS_SECURITY_TOKEN",
		"Authorization",
		"authorization",
		"AK",
		"SK",
	}

	sensitiveSeparators = []string{
		": Bearer ",
		": Basic ",
		"= ",
		": ",
		"=",
		":",
	}
)

func ParseLevel(levelStr string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(levelStr)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// IsSensitiveKey returns true if key matches known credential or authorization header names.
func IsSensitiveKey(key string) bool {
	k := strings.ToUpper(strings.TrimSpace(key))
	switch k {
	case "OS_ACCESS_KEY", "OS_SECRET_KEY", "OS_SECURITY_TOKEN", "AUTHORIZATION", "AK", "SK":
		return true
	default:
		return strings.Contains(k, "SECRET_KEY") ||
			strings.Contains(k, "ACCESS_KEY") ||
			strings.Contains(k, "SECURITY_TOKEN")
	}
}

// RedactAttr sanitizes an individual slog.Attr, masking sensitive key values or inner text.
func RedactAttr(a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, "***")
	}
	return slog.Attr{
		Key:   a.Key,
		Value: redactValue(a.Value),
	}
}

// redactValue recursively redacts supported slog.Value kinds.
func redactValue(val slog.Value) slog.Value {
	switch val.Kind() {
	case slog.KindString:
		return slog.StringValue(RedactString(val.String()))
	case slog.KindGroup:
		groupAttrs := val.Group()
		redacted := make([]slog.Attr, len(groupAttrs))
		for i, gAttr := range groupAttrs {
			redacted[i] = RedactAttr(gAttr)
		}
		return slog.GroupValue(redacted...)
	case slog.KindAny:
		v := val.Any()
		if v == nil {
			return val
		}
		if err, ok := v.(error); ok {
			return slog.StringValue(RedactString(err.Error()))
		}
		if str, ok := v.(fmt.Stringer); ok {
			return slog.StringValue(RedactString(str.String()))
		}
		return val
	default:
		return val
	}
}

// RedactString sanitizes sensitive credentials within a string, masking values to "***".
func RedactString(s string) string {
	if s == "" {
		return s
	}

	res := s
	for _, key := range sensitiveKeys {
		for _, sep := range sensitiveSeparators {
			res = maskKeyValues(res, key+sep)
		}
	}
	return res
}

// maskKeyValues scans s for all instances of prefix (e.g., "OS_SECRET_KEY=") and replaces the
// corresponding value token with "***".
func maskKeyValues(s, prefix string) string {
	idx := 0
	for {
		pos := strings.Index(s[idx:], prefix)
		if pos == -1 {
			break
		}

		start := idx + pos + len(prefix)
		valStart, valEnd := valueBounds(s, start)
		if valEnd > valStart {
			s = s[:valStart] + "***" + s[valEnd:]
			idx = valStart + 3
		} else {
			idx = start
		}
	}
	return s
}

// Spaces and quotes after the prefix are excluded from the value.
func valueBounds(s string, start int) (valStart, valEnd int) {
	valStart = start
	for valStart < len(s) && isQuoteOrSpace(s[valStart]) {
		valStart++
	}

	valEnd = valStart
	for valEnd < len(s) && !isTokenDelimiter(s[valEnd]) {
		valEnd++
	}
	return valStart, valEnd
}

func isQuoteOrSpace(b byte) bool {
	return b == ' ' || b == '"' || b == '\''
}

func isTokenDelimiter(b byte) bool {
	return b == ' ' || b == '"' || b == '\'' || b == '}' || b == ',' || b == ';' || b == '\n'
}

// SanitizingHandler wraps an underlying slog.Handler to redact sensitive credentials.
type SanitizingHandler struct {
	next slog.Handler
}

// NewSanitizingHandler adds credential redaction to next.
func NewSanitizingHandler(next slog.Handler) slog.Handler {
	return &SanitizingHandler{next: next}
}

// Enabled reports whether next accepts records at level.
func (h *SanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle redacts sensitive credentials from record message and attributes before delegating.
func (h *SanitizingHandler) Handle(ctx context.Context, r slog.Record) error {
	newMsg := RedactString(r.Message)
	newRecord := slog.NewRecord(r.Time, r.Level, newMsg, r.PC)

	r.Attrs(func(a slog.Attr) bool {
		newRecord.AddAttrs(RedactAttr(a))
		return true
	})

	return h.next.Handle(ctx, newRecord)
}

// WithAttrs returns a new handler with the given attributes redacted.
func (h *SanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = RedactAttr(a)
	}
	return &SanitizingHandler{next: h.next.WithAttrs(redacted)}
}

// WithGroup preserves redaction within the named group.
func (h *SanitizingHandler) WithGroup(name string) slog.Handler {
	return &SanitizingHandler{next: h.next.WithGroup(name)}
}

// NewLogger creates a new credential-sanitizing *slog.Logger using the provided writer and level.
// If w is nil, os.Stdout is used.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	opts := &slog.HandlerOptions{
		Level: level,
	}
	baseHandler := slog.NewJSONHandler(w, opts)
	return slog.New(NewSanitizingHandler(baseHandler))
}

// NewLoggerFromEnv creates a new *slog.Logger reading the LOG_LEVEL environment variable.
// If w is nil, os.Stdout is used.
func NewLoggerFromEnv(w io.Writer) *slog.Logger {
	levelStr := os.Getenv("LOG_LEVEL")
	level := ParseLevel(levelStr)
	return NewLogger(w, level)
}
