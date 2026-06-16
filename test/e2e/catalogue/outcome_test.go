package catalogue

import (
	"strings"
	"testing"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

// demoCatalogue is a small stand-in whose reachability mix mirrors the
// real one, so the outcome rules are exercised without depending on the
// live catalogue's contents.
var demoCatalogue = []Check{
	{ID: "always-one", Group: "one", Tier: TierForceable, Profiles: proofOnly, Statement: "first"},
	{ID: "always-two", Group: "one", Tier: TierForceable, Profiles: proofOnly, Statement: "second"},
	{ID: "costly", Group: "two", Tier: TierAuthorized, Profiles: proofOnly, Statement: "third"},
	{ID: "racy", Group: "two", Tier: TierOpportunistic, Profiles: proofOnly, Statement: "fourth"},
	{
		ID: "impossible", Group: "two", Tier: TierNotForceable, Profiles: proofOnly,
		Statement: "fifth", ProofRoute: "offline transport",
	},
}

func record(id CheckID, state State, reason SkipReason) Record {
	return Record{ID: id, State: state, Reason: reason}
}

// replace returns records with the entry naming the same check swapped for
// with, so a fixture can alter one outcome without dropping or duplicating
// a check.
func replace(records []Record, with Record) []Record {
	swapped := make([]Record, len(records))
	for i, entry := range records {
		if entry.ID == with.ID {
			entry = with
		}
		swapped[i] = entry
	}
	return swapped
}

func TestReconcileReportsChecksThatNeverRan(t *testing.T) {
	t.Parallel()

	recorded := []Record{record("always-one", StateFailed, ReasonNone)}

	complete := Reconcile(demoCatalogue, recorded)

	if len(complete) != len(demoCatalogue) {
		t.Fatalf(
			"Reconcile length = %d, want one record per selected check %d",
			len(complete),
			len(demoCatalogue),
		)
	}
	for i, entry := range demoCatalogue {
		if complete[i].ID != entry.ID {
			t.Fatalf("record %d names %q, want selected order %q", i, complete[i].ID, entry.ID)
		}
	}
	for _, got := range complete[1:] {
		if got.State != StateNotReached || got.Reason != ReasonNeverRan {
			t.Errorf("%s = %q/%q, want a check that never executed", got.ID, got.State, got.Reason)
		}
	}
}

func TestReconcileKeepsAnUncataloguedRecord(t *testing.T) {
	t.Parallel()

	recorded := []Record{record("stray", StateFailed, ReasonNone)}

	complete := Reconcile(demoCatalogue, recorded)

	var kept bool
	for _, got := range complete {
		if got.ID == "stray" {
			kept = true
		}
	}
	if !kept {
		t.Error("Reconcile dropped a record naming no selected check")
	}
}

func TestReconcileKeepsTheWorstOfDuplicateRecords(t *testing.T) {
	t.Parallel()

	recorded := []Record{
		record("always-one", StateFailed, ReasonNone),
		record("always-one", StateDemonstrated, ReasonNone),
	}

	complete := Reconcile(demoCatalogue, recorded)

	for _, got := range complete {
		if got.ID == "always-one" && got.State != StateFailed {
			t.Errorf("always-one = %q, want the recorded failure kept", got.State)
		}
	}

	result := Summarize(demoCatalogue, complete, false)
	if result.Failed != 1 {
		t.Errorf("Summarize Failed = %d, want the duplicated failure counted", result.Failed)
	}
	if result.ExitCode() != ExitFailed {
		t.Errorf("Summarize ExitCode = %d, want %d", result.ExitCode(), ExitFailed)
	}
}

func TestReconcileFillsSilenceAsNeverRan(t *testing.T) {
	t.Parallel()

	complete := Reconcile(demoCatalogue, nil)

	if len(complete) != len(demoCatalogue) {
		t.Fatalf("Reconcile(nil) length = %d, want %d", len(complete), len(demoCatalogue))
	}
	for _, got := range complete {
		if got.State != StateNotReached || got.Reason != ReasonNeverRan {
			t.Errorf("%s = %q/%q, want never executed", got.ID, got.State, got.Reason)
		}
	}
}

func TestClassifiedReasonNamesAnUnclassifiedSkip(t *testing.T) {
	t.Parallel()

	if got := ClassifiedReason(ReasonNone); got != ReasonUnclassified {
		t.Errorf("ClassifiedReason(ReasonNone) = %q, want %q", got, ReasonUnclassified)
	}
	if got := ClassifiedReason(ReasonWindow); got != ReasonWindow {
		t.Errorf("ClassifiedReason(ReasonWindow) = %q, want %q", got, ReasonWindow)
	}
}

func TestVerdictAndExitCode(t *testing.T) {
	t.Parallel()

	clean := []Record{
		record("always-one", StateDemonstrated, ReasonNone),
		record("always-two", StateDemonstrated, ReasonNone),
		record("costly", StateDemonstrated, ReasonNone),
		record("racy", StateDemonstrated, ReasonNone),
		record("impossible", StateNotReached, ReasonNotForceable),
	}

	cases := []struct {
		name        string
		records     []Record
		strict      bool
		wantVerdict Verdict
		wantExit    int
	}{
		{
			name:        "a clean run is demonstrated",
			records:     clean,
			wantVerdict: VerdictDemonstrated,
			wantExit:    ExitDemonstrated,
		},
		{
			name:        "a clean run stays demonstrated under strict coverage",
			records:     clean,
			strict:      true,
			wantVerdict: VerdictDemonstrated,
			wantExit:    ExitDemonstrated,
		},
		{
			name:        "a failed check fails the run",
			records:     replace(clean, record("always-one", StateFailed, ReasonNone)),
			wantVerdict: VerdictFailed,
			wantExit:    ExitFailed,
		},
		{
			name:        "an exhausted budget is incomplete, not failed",
			records:     replace(clean, record("always-one", StateNotReached, ReasonBudget)),
			wantVerdict: VerdictIncomplete,
			wantExit:    ExitIncomplete,
		},
		{
			name:        "an unauthorized cost is short coverage only under strict",
			records:     replace(clean, record("costly", StateNotReached, ReasonUnauthorized)),
			strict:      true,
			wantVerdict: VerdictIncomplete,
			wantExit:    ExitIncomplete,
		},
		{
			name:        "an unauthorized cost alone is not a failure",
			records:     replace(clean, record("costly", StateNotReached, ReasonUnauthorized)),
			wantVerdict: VerdictDemonstrated,
			wantExit:    ExitDemonstrated,
		},
		{
			name:        "a missed timing window never counts against coverage",
			records:     replace(clean, record("racy", StateNotReached, ReasonWindow)),
			strict:      true,
			wantVerdict: VerdictDemonstrated,
			wantExit:    ExitDemonstrated,
		},
		{
			name:        "a selected check that never ran fails without strict coverage",
			records:     replace(clean, record("always-one", StateNotReached, ReasonNeverRan)),
			wantVerdict: VerdictFailed,
			wantExit:    ExitFailed,
		},
		{
			name:        "an unclassified skip fails without strict coverage",
			records:     replace(clean, record("racy", StateNotReached, ReasonUnclassified)),
			wantVerdict: VerdictFailed,
			wantExit:    ExitFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Summarize(demoCatalogue, tc.records, tc.strict)
			if got.Verdict != tc.wantVerdict {
				t.Errorf("Summarize verdict = %q, want %q", got.Verdict, tc.wantVerdict)
			}
			if got.ExitCode() != tc.wantExit {
				t.Errorf("Summarize ExitCode = %d, want %d", got.ExitCode(), tc.wantExit)
			}
			if got.Strict != tc.strict {
				t.Errorf("Summarize Strict = %v, want %v", got.Strict, tc.strict)
			}
		})
	}
}

func TestStrictCoverageIsSatisfiableWithTheRealCatalogue(t *testing.T) {
	t.Parallel()

	all := SelectCatalogue(settings.ProfileProof)
	var best []Record
	for _, entry := range all {
		if entry.Tier == TierNotForceable {
			best = append(best, record(entry.ID, StateNotReached, ReasonNotForceable))
			continue
		}
		best = append(best, record(entry.ID, StateDemonstrated, ReasonNone))
	}

	got := Summarize(all, best, true)
	if got.ExitCode() != ExitDemonstrated {
		t.Fatalf(
			"the best possible run must satisfy strict coverage, got %q (%d)",
			got.Verdict,
			got.ExitCode(),
		)
	}
	if got.MissingForceable != 0 {
		t.Errorf("MissingForceable = %d, want 0", got.MissingForceable)
	}
	if got.UnclassifiedSkips != 0 {
		t.Errorf("UnclassifiedSkips = %d, want 0", got.UnclassifiedSkips)
	}
}

func TestCoherenceFindings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		records []Record
		want    string
	}{
		{
			name:    "demonstrating the impossible means the catalogue is stale",
			records: []Record{record("impossible", StateDemonstrated, ReasonNone)},
			want:    "stale",
		},
		{
			name:    "a reachable check skipped for cost is a finding",
			records: []Record{record("always-one", StateNotReached, ReasonUnauthorized)},
			want:    "reachable by any run",
		},
		{
			name:    "a costly check skipped for timing is a finding",
			records: []Record{record("costly", StateNotReached, ReasonWindow)},
			want:    "cost authorized",
		},
		{
			name:    "an unclassified skip is a finding even on an opportunistic check",
			records: []Record{record("racy", StateNotReached, ReasonUnclassified)},
			want:    "without classifying",
		},
		{
			name:    "an unclassified skip is a finding on a costly check",
			records: []Record{record("costly", StateNotReached, ReasonUnclassified)},
			want:    "without classifying",
		},
		{
			name:    "a costly check skipped for instance shape is a finding",
			records: []Record{record("costly", StateNotReached, ReasonShape)},
			want:    "cost authorized",
		},
		{
			name:    "a costly check skipped as unforceable is a finding",
			records: []Record{record("costly", StateNotReached, ReasonNotForceable)},
			want:    "cost authorized",
		},
		{
			name:    "an opportunistic check skipped for cost is a finding",
			records: []Record{record("racy", StateNotReached, ReasonUnauthorized)},
			want:    "does not explain",
		},
		{
			name:    "an opportunistic check skipped as unforceable is a finding",
			records: []Record{record("racy", StateNotReached, ReasonNotForceable)},
			want:    "does not explain",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Summarize(demoCatalogue, tc.records, false)
			if got.Verdict != VerdictFailed {
				t.Fatalf("Summarize verdict = %q, want %q", got.Verdict, VerdictFailed)
			}
			entry, found := lookupDemo(tc.records[0].ID)
			if !found {
				t.Fatalf("demo catalogue lost %q", tc.records[0].ID)
			}
			findings := incoherence(entry, tc.records[0])
			if len(findings) == 0 {
				t.Fatalf("expected a finding naming %q, got none", tc.want)
			}
			if !strings.Contains(strings.Join(findings, " "), tc.want) {
				t.Errorf("findings %v do not mention %q", findings, tc.want)
			}
		})
	}
}

func TestCoherentSkipsRaiseNoFinding(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		records     []Record
		wantVerdict Verdict
	}{
		{
			name:        "a forceable check stopped by the budget",
			records:     []Record{record("always-one", StateNotReached, ReasonBudget)},
			wantVerdict: VerdictIncomplete,
		},
		{
			name:        "a forceable check blocked by an earlier failure",
			records:     []Record{record("always-one", StateNotReached, ReasonBlocked)},
			wantVerdict: VerdictDemonstrated,
		},
		{
			name:        "a costly check the operator did not authorize",
			records:     []Record{record("costly", StateNotReached, ReasonUnauthorized)},
			wantVerdict: VerdictDemonstrated,
		},
		{
			name:        "an opportunistic check whose window closed",
			records:     []Record{record("racy", StateNotReached, ReasonWindow)},
			wantVerdict: VerdictDemonstrated,
		},
		{
			name:        "an opportunistic check this instance shape cannot show",
			records:     []Record{record("racy", StateNotReached, ReasonShape)},
			wantVerdict: VerdictDemonstrated,
		},
		{
			name:        "an unforceable check skipped as such",
			records:     []Record{record("impossible", StateNotReached, ReasonNotForceable)},
			wantVerdict: VerdictDemonstrated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Summarize(demoCatalogue, tc.records, false)
			if got.Verdict != tc.wantVerdict {
				t.Errorf("Summarize verdict = %q, want %q", got.Verdict, tc.wantVerdict)
			}
			entry, found := lookupDemo(tc.records[0].ID)
			if !found {
				t.Fatalf("demo catalogue lost %q", tc.records[0].ID)
			}
			findings := incoherence(entry, tc.records[0])
			if len(findings) != 0 {
				t.Errorf("expected no finding, got %v", findings)
			}
		})
	}
}

func TestSummaryCountsUnclassifiedAndMissingForceable(t *testing.T) {
	t.Parallel()

	records := []Record{
		record("always-one", StateNotReached, ReasonNone),
		record("always-two", StateNotReached, ReasonUnclassified),
		record("costly", StateDemonstrated, ReasonNone),
		record("racy", StateNotReached, ReasonWindow),
		record("impossible", StateNotReached, ReasonNotForceable),
	}

	got := Summarize(demoCatalogue, records, false)
	if got.UnclassifiedSkips != 2 {
		t.Errorf("UnclassifiedSkips = %d, want 2", got.UnclassifiedSkips)
	}
	if got.MissingForceable != 2 {
		t.Errorf("MissingForceable = %d, want 2", got.MissingForceable)
	}
	if got.NotReached != 4 {
		t.Errorf("NotReached = %d, want 4", got.NotReached)
	}
	if got.Demonstrated != 1 {
		t.Errorf("Demonstrated = %d, want 1", got.Demonstrated)
	}
	if got.Total != 5 {
		t.Errorf("Total = %d, want 5", got.Total)
	}
	if len(got.Records) != 5 {
		t.Errorf("Records length = %d, want 5", len(got.Records))
	}
}

func TestSummaryExitCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		verdict Verdict
		want    int
	}{
		{verdict: VerdictDemonstrated, want: ExitDemonstrated},
		{verdict: VerdictFailed, want: ExitFailed},
		{verdict: VerdictRefused, want: ExitRefused},
		{verdict: VerdictIncomplete, want: ExitIncomplete},
		{verdict: Verdict("not a verdict"), want: ExitFailed},
	}
	for _, tc := range cases {
		got := Summary{Verdict: tc.verdict}.ExitCode()
		if got != tc.want {
			t.Errorf("Summary{Verdict: %q}.ExitCode() = %d, want %d", tc.verdict, got, tc.want)
		}
	}
}

func lookupDemo(id CheckID) (Check, bool) {
	for _, entry := range demoCatalogue {
		if entry.ID == id {
			return entry, true
		}
	}
	return Check{}, false
}
