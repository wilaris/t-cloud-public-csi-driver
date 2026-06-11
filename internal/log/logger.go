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

// IsSensitiveKey reports whether key is a known credential or auth header name.
func IsSensitiveKey(key string) bool {
	k := strings.ToUpper(strings.TrimSpace(key))
	switch k {
	case "AUTHORIZATION", "AK", "SK":
		return true
	default:
		return strings.Contains(k, "SECRET_KEY") ||
			strings.Contains(k, "ACCESS_KEY") ||
			strings.Contains(k, "SECURITY_TOKEN")
	}
}

// RedactAttr masks sensitive keys and scans string-like values for embedded secrets.
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
	// Resolve LogValuer values before inspecting kind; otherwise secrets surface later unredacted.
	val = val.Resolve()

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
		switch v.(type) {
		case error, fmt.Stringer:
			// Use fmt on error/Stringer so typed nils become "<nil>" instead of panicking.
			return slog.StringValue(RedactString(fmt.Sprintf("%v", v)))
		}
		return val
	default:
		return val
	}
}

// RedactString masks credential values embedded in s (e.g. OS_SECRET_KEY=...).
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

// maskKeyValues replaces values after each prefix match with "***".
// Matches that only end a longer word are skipped (see startsName).
func maskKeyValues(s, prefix string) string {
	idx := 0
	for {
		pos := strings.Index(s[idx:], prefix)
		if pos == -1 {
			break
		}

		keyStart := idx + pos
		if !startsName(s, keyStart) {
			idx = keyStart + 1
			continue
		}

		start := keyStart + len(prefix)
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

// Skip leading spaces/quotes; value runs until a delimiter.
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

// startsName is true when the match at pos is not a suffix of a longer word.
// Letter/digit before pos continues a word ("DISK:" is not "SK"); "_", "-", "." do not.
func startsName(s string, pos int) bool {
	return pos == 0 || !isLetterOrDigit(s[pos-1])
}

func isLetterOrDigit(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func isQuoteOrSpace(b byte) bool {
	return b == ' ' || b == '"' || b == '\''
}

func isTokenDelimiter(b byte) bool {
	return b == ' ' || b == '"' || b == '\'' || b == '}' || b == ',' || b == ';' || b == '\n'
}

// SanitizingHandler redacts secrets in slog records before next.
type SanitizingHandler struct {
	next slog.Handler
}

func NewSanitizingHandler(next slog.Handler) slog.Handler {
	return &SanitizingHandler{next: next}
}

func (h *SanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *SanitizingHandler) Handle(ctx context.Context, r slog.Record) error {
	newMsg := RedactString(r.Message)
	newRecord := slog.NewRecord(r.Time, r.Level, newMsg, r.PC)

	r.Attrs(func(a slog.Attr) bool {
		newRecord.AddAttrs(RedactAttr(a))
		return true
	})

	return h.next.Handle(ctx, newRecord)
}

func (h *SanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = RedactAttr(a)
	}
	return &SanitizingHandler{next: h.next.WithAttrs(redacted)}
}

func (h *SanitizingHandler) WithGroup(name string) slog.Handler {
	return &SanitizingHandler{next: h.next.WithGroup(name)}
}

// NewLogger returns a JSON slog.Logger that redacts credentials. nil w means os.Stdout.
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

// NewLoggerFromEnv is NewLogger with level from LOG_LEVEL (default info).
func NewLoggerFromEnv(w io.Writer) *slog.Logger {
	levelStr := os.Getenv("LOG_LEVEL")
	level := ParseLevel(levelStr)
	return NewLogger(w, level)
}
