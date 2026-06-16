//go:build e2e

package e2e

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opentelekomcloud/gophertelekomcloud/openstack"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/ecs/v1/cloudservers"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/version"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/report"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
)

// backstopGrace is how long past both budgets the run waits before
// abandoning a step that is not answering. A cancellable context cannot
// end a syscall that never returns.
const backstopGrace = time.Minute

// driverVersionTimeout bounds the one call that asks the driver binary
// what build it is.
const driverVersionTimeout = 10 * time.Second

const budgetSkipDetail = "the run passed its own time bound before this check started"

func TestMain(m *testing.M) {
	os.Exit(execute(m))
}

// execute owns the whole run: it refuses an unapproved environment,
// bounds its own wall clock, runs the scenarios, reclaims everything it
// created whether or not they passed and reports what it found.
func execute(m *testing.M) int {
	set, err := settings.LoadSettings(os.Args[1:], os.Getenv)
	switch {
	case errors.Is(err, settings.ErrHelpRequested):
		fmt.Fprint(os.Stdout, settings.UsageText())
		if set != nil && set.HelpAll {
			fmt.Fprintf(os.Stdout, "\nThe test framework also accepts:\n")
			flag.CommandLine.SetOutput(os.Stdout)
			flag.CommandLine.PrintDefaults()
		}
		return catalogue.ExitDemonstrated
	case err != nil:
		fmt.Fprintf(os.Stderr, "refusing to run: %v\n", err)
		return catalogue.ExitRefused
	}

	if set.ListChecks {
		if err := catalogue.WriteCatalogue(
			os.Stdout,
			catalogue.SelectCatalogue(settings.ProfileProof),
		); err != nil {
			fmt.Fprintf(os.Stderr, "print checks: %v\n", err)
			return catalogue.ExitRefused
		}
		return catalogue.ExitDemonstrated
	}
	selected := catalogue.SelectCatalogue(set.Profile)

	runCtx, cancelRun := context.WithTimeout(context.Background(), set.TimeBudget)
	defer cancelRun()

	h, err := preflight(runCtx, set)
	if err != nil {
		fmt.Fprintf(os.Stderr, "refusing to run: %v\n", report.RedactLine(err.Error()))
		return catalogue.ExitRefused
	}
	runState = h
	started := time.Now()

	// A context cannot interrupt a host command that never returns, so
	// the run stops holding the instance either way, having named what
	// it may have left behind.
	backstop := time.AfterFunc(set.TimeBudget+set.TeardownBudget+backstopGrace, func() {
		fmt.Fprintf(
			os.Stderr,
			"abandoning this run past its budgets; resources it recorded are listed in %s\n",
			h.ledger.path,
		)
		if h.progressOut != nil {
			_ = h.progressOut.Flush()
		}
		os.Exit(catalogue.ExitFailed)
	})
	defer backstop.Stop()

	code := m.Run()

	// Reclamation gets its own budget so an expired run can still delete what it created.
	tdCtx, cancelTeardown := context.WithTimeout(context.Background(), set.TeardownBudget)
	defer cancelTeardown()

	// Reclamation failures mean a cloud resource, an attachment or a
	// mount may have survived; a still-running driver counts because it
	// can hold all three. Failures finalizing the run's own records are
	// kept apart so the report never claims a resource survived when
	// none did.
	var reclamation []error
	if h.controller != nil {
		if err := h.controller.stop(tdCtx); err != nil {
			reclamation = append(reclamation, err)
		}
	}
	if h.node != nil {
		if err := h.node.stop(tdCtx); err != nil {
			reclamation = append(reclamation, err)
		}
	}

	reclamation = append(reclamation, h.teardown(tdCtx)...)
	if err := h.sweep(tdCtx); err != nil {
		reclamation = append(reclamation, err)
	}
	var finalization []error
	if h.progressOut != nil {
		if err := h.progressOut.Flush(); err != nil {
			finalization = append(finalization, fmt.Errorf("flush progress: %w", err))
		}
	}
	if err := h.ledger.close(); err != nil {
		finalization = append(finalization, fmt.Errorf("close ledger: %w", err))
	}

	records := reconcileRecords(h, selected)
	h.evidence.Replace(records)
	result := catalogue.Summarize(selected, records, set.Strict)
	if len(reclamation) > 0 || len(finalization) > 0 {
		result.Verdict = catalogue.VerdictFailed
	}
	if code != 0 && result.ExitCode() == catalogue.ExitDemonstrated {
		result.Verdict = catalogue.VerdictFailed
	}

	outputs, err := report.StageFinalOutputs(
		report.ExactMask(secretValues(h.env)),
		h.header,
		records,
		selected,
		result,
		reclamation,
		finalization,
		h.observed(),
		time.Since(started),
		h.evidence,
		h.assertNothingEscaped,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "finalize output: %v\n", err)
		return catalogue.ExitFailed
	}
	if err := report.CommitFinalOutputs(
		h.outputs,
		outputs,
		os.Stdout,
		report.OpenOutput,
	); err != nil {
		fmt.Fprintf(os.Stderr, "commit output: %v\n", err)
		return catalogue.ExitFailed
	}

	return result.ExitCode()
}

// reconcileRecords completes the evidence with one record per selected
// check. Silence after the run clock expired is recorded as a budget
// skip before catalogue.Reconcile fills any remaining gap as never
// executed.
func reconcileRecords(h *harness, selected []catalogue.Check) []catalogue.Record {
	recorded := h.evidence.Records()
	if !h.budgetExhausted() {
		return catalogue.Reconcile(selected, recorded)
	}

	seen := make(map[catalogue.CheckID]bool, len(recorded))
	for _, rec := range recorded {
		seen[rec.ID] = true
	}
	for _, entry := range selected {
		if seen[entry.ID] {
			continue
		}
		recorded = append(recorded, catalogue.Record{
			ID:     entry.ID,
			State:  catalogue.StateNotReached,
			Reason: catalogue.ReasonBudget,
			Detail: budgetSkipDetail,
		})
	}
	return catalogue.Reconcile(selected, recorded)
}

// preflight refuses every condition that would make a run unsafe before
// it creates anything.
func preflight(ctx context.Context, set *settings.Settings) (*harness, error) {
	env, err := loadEnvironment(set, os.Getenv)
	if err != nil {
		return nil, err
	}
	if err := env.assertContained("command line", os.Args...); err != nil {
		return nil, err
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf(
			"the node scenarios attach, format and mount real devices and need a privileged run",
		)
	}

	identity, err := fetchInstanceIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("the metadata service did not identify this instance: %w", err)
	}

	provider, err := evs.NewProviderClient(ctx, env.cloud)
	if err != nil {
		return nil, fmt.Errorf("authenticate against the approved project: %w", err)
	}
	evsClient, err := evs.NewClientFromProvider(provider, env.cloud)
	if err != nil {
		return nil, fmt.Errorf("construct cloud clients: %w", err)
	}
	v3Client, err := openstack.NewBlockStorageV3(provider, golangsdk.EndpointOpts{
		Region: env.cloud.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf("construct volume client: %w", err)
	}
	v2Client, err := openstack.NewBlockStorageV2(provider, golangsdk.EndpointOpts{
		Region: env.cloud.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf("construct volume discovery client: %w", err)
	}
	ecsClient, err := openstack.NewComputeV1(provider, golangsdk.EndpointOpts{
		Region: env.cloud.RegionName,
	})
	if err != nil {
		return nil, fmt.Errorf("construct compute client: %w", err)
	}

	// The approved project must know the instance the metadata document
	// described, so a run cannot drive one project's API from a machine
	// belonging to another.
	server, err := cloudservers.Get(serviceClientWithContext(ctx, ecsClient), identity.ServerID).
		Extract()
	if err != nil {
		return nil, fmt.Errorf(
			"the approved project does not report the instance this run executes on: %w",
			err,
		)
	}

	runID, err := newRunID()
	if err != nil {
		return nil, err
	}
	runDir, err := os.MkdirTemp("", runPrefixRoot+runID+"-")
	if err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}

	recordedLedger, err := newLedger(filepath.Join(runDir, "ledger.jsonl"))
	if err != nil {
		return nil, err
	}

	h := &harness{
		env:      env,
		identity: identity,
		provider: provider,
		evs:      evsClient,
		v3:       v3Client,
		v2:       v2Client,
		ecs:      ecsClient,
		runID:    runID,
		prefix:   runPrefixRoot + runID + "-",
		runDir:   runDir,
		runCtx:   ctx,
		ledger:   recordedLedger,
		selected: catalogue.SelectCatalogue(set.Profile),
	}
	h.selectedIDs = make(map[catalogue.CheckID]bool, len(h.selected))
	for _, entry := range h.selected {
		h.selectedIDs[entry.ID] = true
	}

	h.outputs, err = report.ResolveOutputDestinations(set, runID)
	if err != nil {
		return nil, err
	}
	h.openProgress()

	guest := collectGuestInfo(ctx)
	h.header = buildHeader(ctx, set, env, identity, server.Status, runID, guest)
	h.evidence = report.NewEvidence(h.header)
	h.evidence.DescribeGuest(guest)
	h.evidence.Observe("server_id", identity.ServerID)
	h.evidence.Observe("availability_zone", identity.Zone)
	h.evidence.Observe("server_status", server.Status)

	existing, err := h.snapshotMarkedVolumes(ctx)
	if err != nil {
		return nil, err
	}
	h.preexisting = existing
	h.evidence.Observe("preexisting_marked_volumes", fmt.Sprint(len(existing)))

	if h.controller, err = h.startDriver(ctx, config.RoleController); err != nil {
		return nil, err
	}
	if h.node, err = h.startDriver(ctx, config.RoleNode); err != nil {
		_ = h.controller.stop(ctx)
		return nil, err
	}

	return h, nil
}

// buildHeader records what this run was pointed at, including which
// builds took part.
func buildHeader(
	ctx context.Context,
	set *settings.Settings,
	env *environment,
	identity *instanceIdentity,
	serverStatus string,
	runID string,
	guest report.GuestInfo,
) report.ReportHeader {
	assetBuild := version.Get().String()
	driverBuild := driverIdentity(ctx, env.driverBinary)

	return report.ReportHeader{
		Tool:         settings.ConformanceName,
		ToolBuild:    assetBuild,
		DriverBinary: env.driverBinary,
		DriverBuild:  driverBuild,
		BuildsAgree:  driverBuild == assetBuild,
		Profile:      set.Profile,
		Strict:       set.Strict,
		TimeBudget:   set.TimeBudget,
		RunID:        runID,
		ProjectID:    env.cloud.ProjectID,
		Region:       env.cloud.RegionName,
		Zone:         identity.Zone,
		ServerID:     identity.ServerID,
		ServerStatus: serverStatus,
		VolumeType:   env.volumeType,
		Guest:        guest,
		Started:      time.Now(),
	}
}

// driverIdentity asks the driver binary which build it is, so the
// report can say what it drove. The call carries no cloud variable,
// because reporting a version needs none.
func driverIdentity(ctx context.Context, binary string) string {
	ctx, cancel := context.WithTimeout(ctx, driverVersionTimeout)
	defer cancel()

	//nolint:gosec // the binary path is validated operator input and the argument is fixed here
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LOG_LEVEL=error"}

	out, err := cmd.Output()
	if err != nil {
		return toolUnavailable
	}
	return firstLine(string(out))
}

// openProgress keeps live progress separate from the staged final artifacts.
func (h *harness) openProgress() {
	mask := report.ExactMask(secretValues(h.env))

	h.progressOut = report.NewMaskingWriter(io.MultiWriter(os.Stderr, &h.transcript), mask)
	h.progress = report.NewProgress(h.progressOut, h.selected)
}

// secretValues lists the credential values this run holds so the mask can replace those strings
// before it applies pattern rules.
func secretValues(env *environment) []string {
	var values []string
	for _, secret := range env.secrets() {
		values = append(values, secret)
	}
	return values
}

// observed returns the values the run retained, sorted, for the report.
func (h *harness) observed() []report.NameValue {
	if h.env.settings == nil || !h.env.settings.ShowObservation {
		return nil
	}
	return h.evidence.ObservedPairs()
}

// assertNothingEscaped confirms no credential reached a diagnostic, the
// report or any file the run wrote. The run never writes a credential
// to the instance, so finding one is a failure of the run.
func (h *harness) assertNothingEscaped(label string, staged ...string) error {
	if err := h.env.assertContained("driver diagnostics", h.diagnostics()...); err != nil {
		return err
	}
	if err := h.env.assertContained("progress transcript", h.transcript.String()); err != nil {
		return err
	}
	if err := h.env.assertContained(label, staged...); err != nil {
		return err
	}

	return filepath.WalkDir(h.runDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		//nolint:gosec // the walk is confined to the run's own temporary directory
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return h.env.assertContained("file "+strings.TrimSpace(entry.Name()), string(content))
	})
}
