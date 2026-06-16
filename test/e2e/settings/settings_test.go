package settings

import (
	"errors"
	"flag"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
)

func resolve(t *testing.T, args []string, env map[string]string) (*Settings, error) {
	t.Helper()

	fs := flag.NewFlagSet(ConformanceName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	set := RegisterSettings(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := ResolveSettings(fs, set, lookup(env)); err != nil {
		return nil, err
	}
	return set, nil
}

func load(t *testing.T, args []string, env map[string]string) (*Settings, error) {
	t.Helper()

	fs := flag.NewFlagSet(ConformanceName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return loadSettings(fs, args, lookup(env))
}

func lookup(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func validEnv() map[string]string {
	return map[string]string{
		EnvApprovedProjectID: "00000000000000000000000000000000",
		EnvVolumeType:        "SSD",
	}
}

func TestFlagOverridesEnvironment(t *testing.T) {
	t.Parallel()

	env := validEnv()
	env[EnvVolumeType] = "from-variable"
	env[EnvTimeBudget] = "9m"
	env[EnvStrict] = "false"

	set, err := resolve(
		t,
		[]string{
			"-volume-type=from-flag",
			"-time-budget=12m",
			"-strict",
		},
		env,
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if set.VolumeType != "from-flag" {
		t.Errorf("ResolveSettings volume type = %q, want %q", set.VolumeType, "from-flag")
	}
	if set.TimeBudget != 12*time.Minute {
		t.Errorf("ResolveSettings time budget = %s, want %s", set.TimeBudget, 12*time.Minute)
	}
	if !set.Strict {
		t.Errorf("ResolveSettings strict = %v, want true", set.Strict)
	}
}

func TestEnvironmentOverridesProfileDefault(t *testing.T) {
	t.Parallel()

	env := validEnv()
	env[EnvTimeBudget] = "9m"
	env[EnvTeardownBudget] = "4m"
	env[EnvDriverBinary] = "./custom-driver"
	env[EnvPagingVolumeCount] = "3"
	env[EnvStrict] = "true"

	set, err := resolve(t, []string{"-profile=evaluation"}, env)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if set.TimeBudget != 9*time.Minute {
		t.Errorf("ResolveSettings time budget = %s, want %s", set.TimeBudget, 9*time.Minute)
	}
	if set.TeardownBudget != 4*time.Minute {
		t.Errorf("ResolveSettings teardown budget = %s, want %s", set.TeardownBudget, 4*time.Minute)
	}
	if set.DriverBinary != "./custom-driver" {
		t.Errorf("ResolveSettings driver binary = %q, want %q", set.DriverBinary, "./custom-driver")
	}
	if set.PagingVolumes != 3 {
		t.Errorf("ResolveSettings paging volumes = %d, want 3", set.PagingVolumes)
	}
	if !set.Strict {
		t.Errorf("ResolveSettings strict = %v, want true", set.Strict)
	}
}

func TestWhitespaceEnvironmentIsAbsent(t *testing.T) {
	t.Parallel()

	env := validEnv()
	env[EnvVolumeType] = "   "
	env[EnvTimeBudget] = "\t"
	env[EnvTeardownBudget] = "  "

	set, err := resolve(t, []string{"-profile=evaluation"}, env)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if set.VolumeType != "" {
		t.Errorf(
			"ResolveSettings volume type = %q, want empty after whitespace env",
			set.VolumeType,
		)
	}
	if set.TimeBudget != 45*time.Minute {
		t.Errorf("ResolveSettings time budget = %s, want evaluation default", set.TimeBudget)
	}
	if set.TeardownBudget != 30*time.Minute {
		t.Errorf(
			"ResolveSettings teardown budget = %s, want evaluation default",
			set.TeardownBudget,
		)
	}
}

func TestEachProfileChoosesItsOwnDefaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		args           []string
		wantProfile    Profile
		wantBudget     time.Duration
		wantTeardown   time.Duration
		wantObservings bool
	}{
		{
			name:           "evaluation profile",
			args:           []string{"-profile=evaluation"},
			wantProfile:    ProfileEvaluation,
			wantBudget:     45 * time.Minute,
			wantTeardown:   30 * time.Minute,
			wantObservings: false,
		},
		{
			name:           "proof profile",
			args:           []string{"-profile=proof"},
			wantProfile:    ProfileProof,
			wantBudget:     2 * time.Hour,
			wantTeardown:   30 * time.Minute,
			wantObservings: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			set, err := resolve(t, tc.args, validEnv())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if set.Profile != tc.wantProfile {
				t.Errorf("ResolveSettings profile = %q, want %q", set.Profile, tc.wantProfile)
			}
			if set.TimeBudget != tc.wantBudget {
				t.Errorf("ResolveSettings time budget = %s, want %s", set.TimeBudget, tc.wantBudget)
			}
			if set.TeardownBudget != tc.wantTeardown {
				t.Errorf(
					"ResolveSettings teardown budget = %s, want %s",
					set.TeardownBudget,
					tc.wantTeardown,
				)
			}
			if set.ShowObservation != tc.wantObservings {
				t.Errorf(
					"ResolveSettings show observation = %v, want %v",
					set.ShowObservation,
					tc.wantObservings,
				)
			}
		})
	}
}

func TestDefaultsFor(t *testing.T) {
	t.Parallel()

	eval := DefaultsFor(ProfileEvaluation)
	if eval.TimeBudget != 45*time.Minute || eval.TeardownBudget != 30*time.Minute ||
		eval.ShowObservation {
		t.Errorf("DefaultsFor(evaluation) = %+v, want 45m/30m/false", eval)
	}

	proof := DefaultsFor(ProfileProof)
	if proof.TimeBudget != 2*time.Hour || proof.TeardownBudget != 30*time.Minute ||
		!proof.ShowObservation {
		t.Errorf("DefaultsFor(proof) = %+v, want 2h/30m/true", proof)
	}

	unknown := DefaultsFor(Profile("whatever"))
	if unknown.TimeBudget != eval.TimeBudget || unknown.ShowObservation != eval.ShowObservation {
		t.Errorf("DefaultsFor(unknown) = %+v, want evaluation defaults", unknown)
	}
}

func TestProfileHasNoEnvironmentAlternative(t *testing.T) {
	t.Parallel()

	env := validEnv()
	env["CSI_E2E_PROFILE"] = "proof"
	set, err := resolve(t, nil, env)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if set.Profile != "" {
		t.Errorf("ResolveSettings profile = %q, want no environment selection", set.Profile)
	}
}

func TestDriverBinaryFallsBackBesideTheAsset(t *testing.T) {
	t.Parallel()

	set, err := resolve(t, nil, validEnv())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if set.DriverBinary != DefaultDriverBinary {
		t.Errorf(
			"ResolveSettings driver binary = %q, want %q",
			set.DriverBinary,
			DefaultDriverBinary,
		)
	}
}

func TestEmptyDriverBinaryFlagStillDefaults(t *testing.T) {
	t.Parallel()

	set, err := resolve(t, []string{"-driver-binary="}, validEnv())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if set.DriverBinary != DefaultDriverBinary {
		t.Errorf(
			"ResolveSettings driver binary = %q, want %q",
			set.DriverBinary,
			DefaultDriverBinary,
		)
	}
}

func TestEnvironmentParseErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
		raw  string
		want string
	}{
		{
			name: "time budget is not a duration",
			key:  EnvTimeBudget,
			raw:  "not-a-duration",
			want: EnvTimeBudget + " must be a duration such as 45m",
		},
		{
			name: "teardown budget is not a duration",
			key:  EnvTeardownBudget,
			raw:  "soon",
			want: EnvTeardownBudget + " must be a duration such as 45m",
		},
		{
			name: "paging count is not a number",
			key:  EnvPagingVolumeCount,
			raw:  "abc",
			want: EnvPagingVolumeCount + " must be a non-negative whole number",
		},
		{
			name: "paging count is negative",
			key:  EnvPagingVolumeCount,
			raw:  "-1",
			want: EnvPagingVolumeCount + " must be a non-negative whole number",
		},
		{
			name: "strict is not a bool",
			key:  EnvStrict,
			raw:  "maybe",
			want: EnvStrict + " must be true or false",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := validEnv()
			env[tc.key] = tc.raw
			_, err := resolve(t, []string{"-profile=evaluation"}, env)
			if err == nil {
				t.Fatalf("ResolveSettings() = nil, want error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ResolveSettings() = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRefusedInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "no profile",
			want: "not one this asset declares",
		},
		{
			name: "an unknown profile",
			args: []string{"-profile=whatever"},
			want: "not one this asset declares",
		},
		{
			name: "no asserted project",
			args: []string{"-profile=evaluation"},
			env:  map[string]string{EnvApprovedProjectID: ""},
			want: "asserted as approved",
		},
		{
			name: "no volume type",
			args: []string{"-profile=evaluation"},
			env:  map[string]string{EnvVolumeType: ""},
			want: "no volume type is declared",
		},
		{
			name: "a paging count inside the first page",
			args: []string{"-profile=proof", "-paging-volume-count=1"},
			want: "must exceed the discovery page size",
		},
		{
			name: "a full first-page paging count",
			args: []string{
				"-profile=proof",
				"-paging-volume-count=" + strconv.Itoa(evs.DiscoveryPageSize),
			},
			want: "must exceed the discovery page size",
		},
		{
			name: "a negative paging count",
			args: []string{"-profile=evaluation", "-paging-volume-count=-1"},
			want: "non-negative",
		},
		{
			name: "a run with no time bound",
			args: []string{"-profile=evaluation", "-time-budget=0"},
			want: "unbounded run holds real volumes",
		},
		{
			name: "a run with no teardown budget",
			args: []string{"-profile=evaluation", "-teardown-budget=0"},
			want: "reclamation must be able to finish",
		},
		{
			name: "strict coverage whose cost gate is closed",
			args: []string{"-profile=proof", "-strict"},
			want: "cannot be satisfied",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := validEnv()
			for key, value := range tc.env {
				env[key] = value
			}

			set, err := resolve(t, tc.args, env)
			if err == nil {
				err = ValidateSettings(set)
			}
			if err == nil {
				t.Fatalf("ValidateSettings() = nil, want refusal naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ValidateSettings() = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestStrictCoverageIsAccepted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{
			name: "proof once the cost is authorized",
			args: []string{
				"-profile=proof",
				"-strict",
				"-paging-volume-count=" + strconv.Itoa(evs.DiscoveryPageSize+1),
			},
		},
		{
			name: "evaluation without the proof paging clause",
			args: []string{"-profile=evaluation", "-strict"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			set, err := resolve(t, tc.args, validEnv())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if err := ValidateSettings(set); err != nil {
				t.Fatalf("ValidateSettings() = %v, want strict coverage accepted", err)
			}
		})
	}
}

func TestUnsafeFrameworkFlagsAreRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		defaultValue string
		value        string
	}{
		{name: "test.run", defaultValue: "", value: "Lifecycle"},
		{name: "test.skip", defaultValue: "", value: "Volume"},
		{name: "test.count", defaultValue: "1", value: "2"},
		{name: "test.shuffle", defaultValue: "off", value: "on"},
		{name: "test.failfast", defaultValue: "false", value: "true"},
		{name: "test.timeout", defaultValue: "0s", value: "1m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fs := flag.NewFlagSet(ConformanceName, flag.ContinueOnError)
			fs.String(tc.name, tc.defaultValue, "")
			if err := fs.Set(tc.name, tc.value); err != nil {
				t.Fatalf("set %s: %v", tc.name, err)
			}
			err := ValidateFrameworkFlags(fs)
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Errorf("ValidateFrameworkFlags(%s) = %v, want refusal", tc.name, err)
			}
		})
	}
}

func TestFrameworkFlagDefaultsAreAccepted(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet(ConformanceName, flag.ContinueOnError)
	fs.String("test.run", "", "")
	fs.String("test.skip", "", "")
	fs.String("test.count", "1", "")
	fs.String("test.shuffle", "off", "")
	fs.String("test.failfast", "false", "")
	fs.String("test.timeout", "0s", "")

	if err := ValidateFrameworkFlags(fs); err != nil {
		t.Fatalf("ValidateFrameworkFlags() = %v, want nil for defaults", err)
	}
}

func TestMissingFrameworkFlagsAreAccepted(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet(ConformanceName, flag.ContinueOnError)
	if err := ValidateFrameworkFlags(fs); err != nil {
		t.Fatalf("ValidateFrameworkFlags() = %v, want nil when flags are absent", err)
	}
}

func TestLoadPath(t *testing.T) {
	t.Parallel()

	t.Run("help flag", func(t *testing.T) {
		t.Parallel()

		set, err := load(t, []string{"-h"}, nil)
		if !errors.Is(err, ErrHelpRequested) {
			t.Fatalf("loadSettings(-h) err = %v, want ErrHelpRequested", err)
		}
		if set == nil {
			t.Fatal("loadSettings(-h) set = nil, want parsed settings")
		}
	})

	t.Run("help-all", func(t *testing.T) {
		t.Parallel()

		set, err := load(t, []string{"-help-all"}, nil)
		if !errors.Is(err, ErrHelpRequested) {
			t.Fatalf("loadSettings(-help-all) err = %v, want ErrHelpRequested", err)
		}
		if set == nil || !set.HelpAll {
			t.Fatalf("loadSettings(-help-all) set = %+v, want HelpAll", set)
		}
	})

	t.Run("list-checks skips validation", func(t *testing.T) {
		t.Parallel()

		set, err := load(t, []string{"-list-checks"}, nil)
		if err != nil {
			t.Fatalf("loadSettings(-list-checks) = %v, want nil", err)
		}
		if !set.ListChecks {
			t.Errorf("loadSettings(-list-checks) ListChecks = false, want true")
		}
	})

	t.Run("leftover positional", func(t *testing.T) {
		t.Parallel()

		_, err := load(t, []string{"leftover"}, validEnv())
		if err == nil || !strings.Contains(err.Error(), "is not a setting this asset accepts") {
			t.Fatalf("loadSettings(leftover) = %v, want positional refusal", err)
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		t.Parallel()

		_, err := load(t, []string{"-not-a-setting"}, nil)
		if err == nil {
			t.Fatal("loadSettings(-not-a-setting) = nil, want parse error")
		}
		if !strings.Contains(err.Error(), "not-a-setting") {
			t.Errorf("loadSettings(-not-a-setting) = %q, want the flag name", err)
		}
	})

	t.Run("accepted evaluation run", func(t *testing.T) {
		t.Parallel()

		set, err := load(t, []string{"-profile=evaluation"}, validEnv())
		if err != nil {
			t.Fatalf("loadSettings(evaluation) = %v, want nil", err)
		}
		if set.Profile != ProfileEvaluation {
			t.Errorf(
				"loadSettings(evaluation) profile = %q, want %q",
				set.Profile,
				ProfileEvaluation,
			)
		}
		if set.Approved != validEnv()[EnvApprovedProjectID] {
			t.Errorf("loadSettings(evaluation) approved = %q, want env value", set.Approved)
		}
		if set.TimeBudget != 45*time.Minute {
			t.Errorf("loadSettings(evaluation) time budget = %s, want 45m", set.TimeBudget)
		}
	})
}

func TestParseErrorRedactsSecrets(t *testing.T) {
	t.Parallel()

	const secret = "supersecret-credential-value"
	_, err := load(t, []string{"OS_SECRET_KEY=" + secret}, nil)
	if err == nil {
		t.Fatal("loadSettings() = nil, want positional refusal")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("loadSettings() leaked secret in %q", err)
	}
	if !strings.Contains(err.Error(), "OS_SECRET_KEY=***") {
		t.Errorf("loadSettings() = %q, want redacted secret marker", err)
	}
}

func TestUsageStatesTheCostAndTheExitCodes(t *testing.T) {
	t.Parallel()

	text := UsageText()
	required := []string{
		"root",
		"bills real storage",
		"Exit codes",
		"-profile",
		"-approved-project-id",
		"-volume-type",
		"-driver-binary",
		"-evidence-path",
		"-report-path",
		"-paging-volume-count",
		"-time-budget",
		"-teardown-budget",
		"-strict",
		"-list-checks",
		"-help-all",
		EnvApprovedProjectID,
		EnvVolumeType,
		EnvDriverBinary,
		EnvEvidencePath,
		EnvReportPath,
		EnvPagingVolumeCount,
		EnvTimeBudget,
		EnvTeardownBudget,
		EnvStrict,
		ConformanceName,
	}
	for _, expected := range required {
		if !strings.Contains(text, expected) {
			t.Errorf("UsageText() does not mention %q", expected)
		}
	}
}

func TestLoadSettingsBindsCommandLine(t *testing.T) {
	set, err := LoadSettings([]string{"-list-checks"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadSettings(-list-checks) = %v, want nil", err)
	}
	if !set.ListChecks {
		t.Errorf("LoadSettings(-list-checks) ListChecks = false, want true")
	}
}
