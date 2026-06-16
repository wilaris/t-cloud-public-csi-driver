package report

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	driverlog "git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
)

// GuestInfo records the host tools a run used, so the report can show how this guest differs from
// the node image a release targets.
type GuestInfo struct {
	// Kernel is the guest kernel version the run observed.
	Kernel string
	// Udev is the udev version the run observed.
	Udev string
	// Blkid is the blkid version the run observed.
	Blkid string
	// MkfsExt4 is the mkfs.ext4 version the run observed.
	MkfsExt4 string
	// MkfsXfs is the mkfs.xfs version the run observed.
	MkfsXfs string
}

// Evidence is the machine-readable record a run emits. It is written
// through the driver's own sanitizing handler, so no credential can
// reach it even when an observed value carries one; the caller
// additionally routes the writer through the run's exact-value mask.
type Evidence struct {
	header ReportHeader

	mu           sync.Mutex
	guest        GuestInfo
	clauses      []catalogue.Record
	observations map[string]string
}

// NewEvidence creates the in-memory record that final output renders
// once the run is complete. Run identity comes from the header.
func NewEvidence(h ReportHeader) *Evidence {
	return &Evidence{
		header:       h,
		observations: map[string]string{},
	}
}

// DescribeGuest records the host facilities the run exercised.
func (e *Evidence) DescribeGuest(g GuestInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.guest = g
}

// Observe retains a server-issued or host-observed value the checks
// assert against, so a later disagreement about what the service
// answered can be settled.
func (e *Evidence) Observe(name, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observations[name] = value
}

// Record appends one check outcome to the in-memory record.
func (e *Evidence) Record(r catalogue.Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clauses = append(e.clauses, r)
}

// ObservedPairs returns the retained values in a stable order, for the readable report.
func (e *Evidence) ObservedPairs() []NameValue {
	e.mu.Lock()
	defer e.mu.Unlock()

	keys := make([]string, 0, len(e.observations))
	for key := range e.observations {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	pairs := make([]NameValue, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, NameValue{Name: key, Value: e.observations[key]})
	}
	return pairs
}

// Records returns a copy of what the run claimed, for reconciliation and the report.
func (e *Evidence) Records() []catalogue.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]catalogue.Record(nil), e.clauses...)
}

// Replace installs the reconciled record set, so the emitted record and the report agree.
func (e *Evidence) Replace(records []catalogue.Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clauses = append([]catalogue.Record(nil), records...)
}

// Render writes the one machine-readable record and returns any handler or destination error.
func (e *Evidence) Render(
	w io.Writer,
	selected []catalogue.Check,
	result catalogue.Summary,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	index := make(map[catalogue.CheckID]catalogue.Check, len(selected))
	for _, entry := range selected {
		index[entry.ID] = entry
	}

	clauseAttrs := make([]any, 0, len(e.clauses))
	reasonCounts := map[catalogue.SkipReason]int{}
	for _, clause := range e.clauses {
		if clause.State == catalogue.StateNotReached {
			reasonCounts[clause.Reason]++
		}
		entry, known := index[clause.ID]
		fields := []any{
			slog.String("state", string(clause.State)),
			slog.Int64("elapsed_ms", clause.Elapsed.Milliseconds()),
		}
		if known {
			fields = append(fields,
				slog.String("scenario", entry.Group),
				slog.String("reachable", string(entry.Tier)),
				slog.String("statement", entry.Statement),
			)
			if entry.ProofRoute != "" {
				fields = append(fields, slog.String("proof_route", entry.ProofRoute))
			}
		}
		if clause.Reason != catalogue.ReasonNone {
			fields = append(fields, slog.String("reason", string(clause.Reason)))
		}
		if clause.Detail != "" {
			fields = append(fields, slog.String("detail", clause.Detail))
		}
		clauseAttrs = append(clauseAttrs, slog.Group(string(clause.ID), fields...))
	}

	keys := make([]string, 0, len(e.observations))
	for key := range e.observations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	observationAttrs := make([]any, 0, len(keys))
	for _, key := range keys {
		observationAttrs = append(observationAttrs, slog.String(key, e.observations[key]))
	}

	reasonAttrs := make([]any, 0, len(reasonCounts))
	reasons := make([]string, 0, len(reasonCounts))
	for reason := range reasonCounts {
		reasons = append(reasons, string(reason))
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		reasonAttrs = append(
			reasonAttrs,
			slog.Int(reason, reasonCounts[catalogue.SkipReason(reason)]),
		)
	}

	handler := driverlog.NewSanitizingHandler(
		slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}),
	)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "End-to-end run evidence", 0)
	record.AddAttrs(
		slog.String("run_id", e.header.RunID),
		slog.String("approved_project_id", e.header.ProjectID),
		slog.Group("build",
			slog.String("asset", e.header.ToolBuild),
			slog.String("driver", e.header.DriverBuild),
			slog.Bool("agree", e.header.BuildsAgree),
		),
		slog.Group("run",
			slog.String("profile", string(e.header.Profile)),
			slog.Bool("strict", e.header.Strict),
			slog.String("time_budget", e.header.TimeBudget.String()),
			slog.String("region", e.header.Region),
			slog.String("volume_type", e.header.VolumeType),
		),
		slog.Group("guest",
			slog.String("kernel", e.guest.Kernel),
			slog.String("udev", e.guest.Udev),
			slog.String("blkid", e.guest.Blkid),
			slog.String("mkfs_ext4", e.guest.MkfsExt4),
			slog.String("mkfs_xfs", e.guest.MkfsXfs),
		),
		slog.Group("summary",
			slog.Int("total", result.Total),
			slog.Int("demonstrated", result.Demonstrated),
			slog.Int("failed", result.Failed),
			slog.Int("not_reached", result.NotReached),
			slog.Group("not_reached_because", reasonAttrs...),
			slog.String("verdict", string(result.Verdict)),
			slog.Int("exit", result.ExitCode()),
		),
		slog.Group("clauses", clauseAttrs...),
		slog.Group("observations", observationAttrs...),
	)
	return handler.Handle(context.Background(), record)
}
