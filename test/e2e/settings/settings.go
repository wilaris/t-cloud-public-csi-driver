// Package settings resolves the live proof asset's non-credential run input.
//
// A run is configured by flags, then environment variables, then the selected
// profile's defaults. The profile itself is a required flag and has no
// environment alternative. Credentials are not accepted here.
package settings

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	driverlog "git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
)

const (
	// ConformanceName is the asset's own name. It appears in usage and report headers.
	ConformanceName = "t-cloud-csi-conformance"

	// DefaultDriverBinary is the driver path used when neither flag nor variable names one.
	DefaultDriverBinary = "./t-cloud-csi-driver"
)

const (
	// EnvApprovedProjectID is the operator's assertion that the configured project may be mutated.
	// Approval is asserted; it is never inferred from a project name or tag.
	EnvApprovedProjectID = "CSI_E2E_APPROVED_PROJECT_ID"
	// EnvVolumeType names the volume type to provision. Types are regional and this asset
	// carries no inventory, so the operator declares one.
	EnvVolumeType = "CSI_E2E_VOLUME_TYPE"
	// EnvDriverBinary locates the driver binary started as the controller and node roles.
	EnvDriverBinary = "CSI_E2E_DRIVER_BINARY"
	// EnvEvidencePath optionally replaces the run-identifier-named JSON evidence path.
	EnvEvidencePath = "CSI_E2E_EVIDENCE_PATH"
	// EnvReportPath optionally writes the readable report to a file instead of standard output.
	EnvReportPath = "CSI_E2E_REPORT_PATH"
	// EnvPagingVolumeCount authorizes the one check whose cost is unreasonable by default.
	// The value must exceed the driver's discovery page size.
	EnvPagingVolumeCount = "CSI_E2E_PAGING_VOLUME_COUNT"
	// EnvTimeBudget bounds the whole run's wall clock.
	EnvTimeBudget = "CSI_E2E_TIME_BUDGET"
	// EnvTeardownBudget bounds reclamation separately from the run's own bound.
	EnvTeardownBudget = "CSI_E2E_TEARDOWN_BUDGET"
	// EnvStrict requires every check the profile selected to be reached.
	EnvStrict = "CSI_E2E_STRICT"
)

// Profile selects one audience's catalogue membership and defaults.
type Profile string

const (
	// ProfileEvaluation answers someone deciding whether the driver works in their own project.
	ProfileEvaluation Profile = "evaluation"
	// ProfileProof answers whoever maintains the driver and wants the machine-readable record.
	ProfileProof Profile = "proof"
)

// ErrHelpRequested reports that the caller asked for usage, not for a run.
var ErrHelpRequested = errors.New("usage requested")

// Settings is one run's non-credential input after flags, environment variables and profile
// defaults have been resolved. No credential is accepted here.
type Settings struct {
	// Profile is the declared audience. It is required as a flag.
	Profile Profile
	// Approved is the project identifier the operator asserts may be mutated.
	Approved string
	// VolumeType is the regional volume type to provision.
	VolumeType string
	// DriverBinary locates the driver executable started as controller and node.
	DriverBinary string
	// EvidencePath optionally replaces the run-identifier-named JSON evidence file.
	EvidencePath string
	// ReportPath optionally writes the readable report to a file instead of standard output.
	ReportPath string
	// PagingVolumes authorizes the extra-cost later-page check when it exceeds the
	// discovery page size. Zero leaves that check unauthorized.
	PagingVolumes int
	// TimeBudget bounds the run's wall clock.
	TimeBudget time.Duration
	// TeardownBudget bounds reclamation separately from TimeBudget.
	TeardownBudget time.Duration
	// Strict fails the run when a selected check was not reached.
	Strict bool
	// ListChecks prints the catalogue and exits without reaching the cloud.
	ListChecks bool
	// HelpAll asks for usage including the test framework's own flags.
	HelpAll bool
	// ShowObservation is chosen by the profile and is not operator-set.
	ShowObservation bool
}

// ProfileDefaults are the values a profile chooses when neither a flag nor a variable did.
type ProfileDefaults struct {
	// TimeBudget is the profile's wall-clock bound.
	TimeBudget time.Duration
	// TeardownBudget is the profile's reclamation bound.
	TeardownBudget time.Duration
	// ShowObservation reports whether the profile includes observed values in the record.
	ShowObservation bool
}

// DefaultsFor returns the profile's built-in defaults. An unrecognized profile receives
// the evaluation defaults so validation, not defaulting, is what refuses it.
func DefaultsFor(p Profile) ProfileDefaults {
	if p == ProfileProof {
		return ProfileDefaults{
			TimeBudget:      2 * time.Hour,
			TeardownBudget:  30 * time.Minute,
			ShowObservation: true,
		}
	}
	return ProfileDefaults{
		TimeBudget:     45 * time.Minute,
		TeardownBudget: 30 * time.Minute,
	}
}

// settingSpec declares one run setting exactly once. The flag registry, the variable
// resolution and the usage block all walk the same list so they cannot drift apart.
type settingSpec struct {
	flag  string
	env   string
	usage string
	// register declares the flag. It receives the spec so the name and usage are stated once.
	register func(fs *flag.FlagSet, set *Settings, spec settingSpec)
	// applyEnv applies the variable's raw value when the flag was not given. It is nil for flag-only
	// settings.
	applyEnv func(set *Settings, spec settingSpec, raw string) error
}

func stringSetting(field func(*Settings) *string) settingSpec {
	return settingSpec{
		register: func(fs *flag.FlagSet, set *Settings, spec settingSpec) {
			fs.StringVar(field(set), spec.flag, "", spec.usage)
		},
		applyEnv: func(set *Settings, _ settingSpec, raw string) error {
			*field(set) = raw
			return nil
		},
	}
}

func durationSetting(field func(*Settings) *time.Duration) settingSpec {
	return settingSpec{
		register: func(fs *flag.FlagSet, set *Settings, spec settingSpec) {
			fs.DurationVar(field(set), spec.flag, 0, spec.usage)
		},
		applyEnv: func(set *Settings, spec settingSpec, raw string) error {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("%s must be a duration such as 45m", spec.env)
			}
			*field(set) = parsed
			return nil
		},
	}
}

// settingSpecs is the one list of run settings, in the order the usage block prints them.
var settingSpecs = buildSettingSpecs()

func buildSettingSpecs() []settingSpec {
	named := func(spec settingSpec, name, env, usage string) settingSpec {
		spec.flag, spec.env, spec.usage = name, env, usage
		return spec
	}

	return []settingSpec{
		{
			flag:  "profile",
			usage: "required declared audience: evaluation or proof",
			register: func(fs *flag.FlagSet, set *Settings, spec settingSpec) {
				fs.StringVar((*string)(&set.Profile), spec.flag, "", spec.usage)
			},
		},
		named(stringSetting(func(set *Settings) *string { return &set.Approved }),
			"approved-project-id", EnvApprovedProjectID,
			"the project this run may mutate; required"),
		named(stringSetting(func(set *Settings) *string { return &set.VolumeType }),
			"volume-type", EnvVolumeType,
			"the volume type to provision; required and regional"),
		named(stringSetting(func(set *Settings) *string { return &set.DriverBinary }),
			"driver-binary", EnvDriverBinary,
			"the driver binary to drive; defaults to "+DefaultDriverBinary),
		named(stringSetting(func(set *Settings) *string { return &set.EvidencePath }),
			"evidence-path", EnvEvidencePath,
			"replace the run-identifier-named JSON evidence path"),
		named(stringSetting(func(set *Settings) *string { return &set.ReportPath }),
			"report-path", EnvReportPath,
			"write the readable report here instead of standard output"),
		{
			flag:  "paging-volume-count",
			env:   EnvPagingVolumeCount,
			usage: "authorize the one check that costs extra volumes",
			register: func(fs *flag.FlagSet, set *Settings, spec settingSpec) {
				fs.IntVar(&set.PagingVolumes, spec.flag, 0, spec.usage)
			},
			applyEnv: func(set *Settings, _ settingSpec, raw string) error {
				count, err := strconv.Atoi(raw)
				if err != nil || count < 0 {
					return fmt.Errorf(
						"%s must be a non-negative whole number",
						EnvPagingVolumeCount,
					)
				}
				set.PagingVolumes = count
				return nil
			},
		},
		named(durationSetting(func(set *Settings) *time.Duration { return &set.TimeBudget }),
			"time-budget", EnvTimeBudget,
			"stop the run after this much wall clock"),
		named(durationSetting(func(set *Settings) *time.Duration { return &set.TeardownBudget }),
			"teardown-budget", EnvTeardownBudget,
			"how long reclamation may take"),
		{
			flag:  "strict",
			env:   EnvStrict,
			usage: "fail when a selected check was not reached",
			register: func(fs *flag.FlagSet, set *Settings, spec settingSpec) {
				fs.BoolVar(&set.Strict, spec.flag, false, spec.usage)
			},
			applyEnv: func(set *Settings, _ settingSpec, raw string) error {
				parsed, err := strconv.ParseBool(raw)
				if err != nil {
					return fmt.Errorf("%s must be true or false", EnvStrict)
				}
				set.Strict = parsed
				return nil
			},
		},
		{
			flag:  "list-checks",
			usage: "print what this asset checks and exit",
			register: func(fs *flag.FlagSet, set *Settings, spec settingSpec) {
				fs.BoolVar(&set.ListChecks, spec.flag, false, spec.usage)
			},
		},
		{
			flag:  "help-all",
			usage: "also print the test framework's own flags",
			register: func(fs *flag.FlagSet, set *Settings, spec settingSpec) {
				fs.BoolVar(&set.HelpAll, spec.flag, false, spec.usage)
			},
		},
	}
}

// RegisterSettings declares every run setting on fs and returns the struct they land in.
func RegisterSettings(fs *flag.FlagSet) *Settings {
	set := &Settings{}
	for _, spec := range settingSpecs {
		spec.register(fs, set, spec)
	}
	return set
}

// ResolveSettings fills each setting the command line left alone: its variable first, the selected
// profile's default second. A flag therefore wins over its variable. A variable wins over a default.
func ResolveSettings(fs *flag.FlagSet, set *Settings, getenv func(string) string) error {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	value := func(name string) string { return strings.TrimSpace(getenv(name)) }

	for _, spec := range settingSpecs {
		if spec.applyEnv == nil || given[spec.flag] {
			continue
		}
		raw := value(spec.env)
		if raw == "" {
			continue
		}
		if err := spec.applyEnv(set, spec, raw); err != nil {
			return err
		}
	}

	// Profile defaults apply only where neither a flag nor a variable said anything.
	defaults := DefaultsFor(set.Profile)
	if !given["time-budget"] && value(EnvTimeBudget) == "" {
		set.TimeBudget = defaults.TimeBudget
	}
	if !given["teardown-budget"] && value(EnvTeardownBudget) == "" {
		set.TeardownBudget = defaults.TeardownBudget
	}
	if set.DriverBinary == "" {
		set.DriverBinary = DefaultDriverBinary
	}
	set.ShowObservation = defaults.ShowObservation

	return nil
}

// ValidateSettings rejects input that cannot start a run, without touching the filesystem or the
// cloud.
func ValidateSettings(set *Settings) error {
	switch set.Profile {
	case ProfileEvaluation, ProfileProof:
	default:
		return fmt.Errorf(
			"profile %q is not one this asset declares; use %q or %q",
			set.Profile,
			ProfileEvaluation,
			ProfileProof,
		)
	}

	if set.Approved == "" {
		return fmt.Errorf(
			"no project is asserted as approved; set -approved-project-id or %s",
			EnvApprovedProjectID,
		)
	}
	if set.VolumeType == "" {
		return fmt.Errorf(
			"no volume type is declared; available types are regional, so set -volume-type or %s",
			EnvVolumeType,
		)
	}
	if set.PagingVolumes < 0 {
		return fmt.Errorf("-paging-volume-count must be a non-negative whole number")
	}
	if set.PagingVolumes > 0 && set.PagingVolumes <= evs.DiscoveryPageSize {
		return fmt.Errorf(
			"-paging-volume-count must exceed the discovery page size of %d",
			evs.DiscoveryPageSize,
		)
	}
	if set.TimeBudget <= 0 {
		return fmt.Errorf(
			"-time-budget must be positive, because an unbounded run holds real volumes",
		)
	}
	if set.TeardownBudget <= 0 {
		return fmt.Errorf(
			"-teardown-budget must be positive, because reclamation must be able to finish",
		)
	}

	// A strict proof run requires every selected check. The paged-discovery check cannot
	// be reached without the cost opt-in. Refusing now beats discovering it after an hour of billing.
	if set.Profile == ProfileProof && set.Strict && set.PagingVolumes == 0 {
		return fmt.Errorf(
			"-strict cannot be satisfied while -paging-volume-count is unset, because one check provisions more volumes than a discovery page holds",
		)
	}

	return nil
}

// LoadSettings resolves one run's non-credential input from args. It shares the flag set
// the test framework registered its own flags on, so both parse in one pass. It
// converts that set to report a parse failure instead of exiting inside the flag package.
func LoadSettings(args []string, getenv func(string) string) (*Settings, error) {
	flag.CommandLine.Init(ConformanceName, flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	flag.Usage = func() {}
	return loadSettings(flag.CommandLine, args, getenv)
}

// loadSettings is the testable load path. Callers that must share the process flag set
// go through LoadSettings; tests pass a private FlagSet so they do not touch CommandLine.
func loadSettings(fs *flag.FlagSet, args []string, getenv func(string) string) (*Settings, error) {
	set := RegisterSettings(fs)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return set, ErrHelpRequested
		}
		return nil, errors.New(driverlog.RedactString(err.Error()))
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf(
			"%q is not a setting this asset accepts",
			driverlog.RedactString(fs.Arg(0)),
		)
	}
	if set.HelpAll {
		return set, ErrHelpRequested
	}
	// Listing what the asset checks reaches no cloud and mutates nothing, so it must not
	// require the approval assertion or a credential that only a real run needs.
	if set.ListChecks {
		return set, nil
	}

	if err := ValidateFrameworkFlags(fs); err != nil {
		return nil, err
	}

	if err := ResolveSettings(fs, set, getenv); err != nil {
		return nil, err
	}
	if err := ValidateSettings(set); err != nil {
		return nil, err
	}
	return set, nil
}

// ValidateFrameworkFlags refuses controls that can skip, repeat, reorder or prematurely
// stop cloud effects outside the selected profile.
func ValidateFrameworkFlags(fs *flag.FlagSet) error {
	defaults := []struct {
		name  string
		value string
	}{
		{name: "test.run", value: ""},
		{name: "test.skip", value: ""},
		{name: "test.count", value: "1"},
		{name: "test.shuffle", value: "off"},
		{name: "test.failfast", value: "false"},
		{name: "test.timeout", value: "0s"},
	}
	for _, entry := range defaults {
		f := fs.Lookup(entry.name)
		if f == nil || f.Value.String() == entry.value {
			continue
		}
		return fmt.Errorf(
			"the test framework setting -%s=%s cannot alter a cloud-mutating run; select work with -profile",
			entry.name,
			driverlog.RedactString(f.Value.String()),
		)
	}
	return nil
}

// UsageText returns the curated settings block. The test framework's own flags are omitted: they
// are not this asset's surface.
func UsageText() string {
	var out strings.Builder

	fmt.Fprintf(
		&out,
		"%s checks whether this driver works against one cloud project.\n\n",
		ConformanceName,
	)
	out.WriteString(
		"It creates, attaches, formats, mounts and deletes real volumes in the project you\n",
	)
	out.WriteString(
		"name, bills real storage and must run as root on an instance in that project.\n\n",
	)
	fmt.Fprintf(&out, "Usage: %s [settings]\n\n", ConformanceName)
	out.WriteString(
		"Settings. A flag wins over its variable. A variable wins over the selected profile's default.\n\n",
	)

	for _, spec := range settingSpecs {
		fmt.Fprintf(&out, "  %-22s %-30s %s\n", "-"+spec.flag, spec.env, spec.usage)
	}

	out.WriteString(
		"\nCloud credentials are read from the process environment only. They are never\n",
	)
	out.WriteString(
		"accepted as a flag. A run refuses to start if one appears on its command line.\n",
	)
	out.WriteString(
		"Test-framework filters, skips, repetition, shuffling, fail-fast and timeout controls are refused.\n",
	)
	out.WriteString(
		"\nExit codes: 0 demonstrated, 1 a check failed or a resource survived, 2 refused\n",
	)
	out.WriteString(
		"before anything was created, 3 finished without covering everything selected.\n",
	)

	return out.String()
}
