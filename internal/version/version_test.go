package version_test

import (
	"runtime"
	"strings"
	"testing"

	"wilaris.dev/t-cloud-public-csi-driver/internal/version"
)

func TestGetPopulatesEveryField(t *testing.T) {
	t.Parallel()

	info := version.Get()

	fields := map[string]string{
		"Version":   info.Version,
		"Commit":    info.Commit,
		"BuildDate": info.BuildDate,
		"GoVersion": info.GoVersion,
		"Platform":  info.Platform,
	}
	for name, value := range fields {
		if value == "" {
			t.Errorf("expected %s to be populated, got an empty string", name)
		}
	}
}

func TestGetReportsTheRunningToolchainAndTarget(t *testing.T) {
	t.Parallel()

	info := version.Get()

	if info.GoVersion != runtime.Version() {
		t.Errorf("expected GoVersion %q, got %q", runtime.Version(), info.GoVersion)
	}
	wantPlatform := runtime.GOOS + "/" + runtime.GOARCH
	if info.Platform != wantPlatform {
		t.Errorf("expected Platform %q, got %q", wantPlatform, info.Platform)
	}
}

// go test never stamps, so these are the values a binary reports when the linker flags miss.
func TestUnstampedBuildReportsPlaceholders(t *testing.T) {
	t.Parallel()

	info := version.Get()

	if info.Version != "dev" {
		t.Errorf("expected an unstamped Version %q, got %q", "dev", info.Version)
	}
	if info.Commit != "unknown" {
		t.Errorf("expected an unstamped Commit %q, got %q", "unknown", info.Commit)
	}
	if info.BuildDate != "unknown" {
		t.Errorf("expected an unstamped BuildDate %q, got %q", "unknown", info.BuildDate)
	}
}

func TestVersionMatchesTheReportedInfo(t *testing.T) {
	t.Parallel()

	if version.Version() != version.Get().Version {
		t.Errorf(
			"expected Version %q to match Get().Version %q",
			version.Version(),
			version.Get().Version,
		)
	}
}

func TestStringSummarizesTheBuildOnOneLine(t *testing.T) {
	t.Parallel()

	info := version.Get()
	summary := info.String()

	if strings.Contains(summary, "\n") {
		t.Errorf("expected a single-line summary, got %q", summary)
	}
	for _, want := range []string{info.Version, info.Commit, info.GoVersion, info.Platform} {
		if !strings.Contains(summary, want) {
			t.Errorf("expected the summary to contain %q, got %q", want, summary)
		}
	}
}
