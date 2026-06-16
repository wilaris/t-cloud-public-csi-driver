package report

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

// privateIdentifier matches the control-repository identifier shapes the
// repository gate forbids anywhere in this public tree. Asserting it here
// turns a gate failure into a unit-test failure.
var privateIdentifier = regexp.MustCompile(`(AC|NFR|IR|ADR|B|F|[A-Z0-9]+-(FR|D))-[0-9]+`)

// demoCatalogue is a small stand-in whose reachability mix mirrors the
// real one, so report tests do not depend on the live catalogue's contents.
var demoCatalogue = []catalogue.Check{
	{ID: "always-one", Group: "one", Tier: catalogue.TierForceable, Statement: "first"},
	{ID: "always-two", Group: "one", Tier: catalogue.TierForceable, Statement: "second"},
	{ID: "costly", Group: "two", Tier: catalogue.TierAuthorized, Statement: "third"},
	{ID: "racy", Group: "two", Tier: catalogue.TierOpportunistic, Statement: "fourth"},
	{
		ID: "impossible", Group: "two", Tier: catalogue.TierNotForceable,
		Statement: "fifth", ProofRoute: "offline transport",
	},
}

func demoHeader() ReportHeader {
	return ReportHeader{
		Tool:         settings.ConformanceName,
		ToolBuild:    "v0.1.0 (abc1234)",
		DriverBinary: "./t-cloud-csi-driver",
		DriverBuild:  "v0.1.0 (abc1234)",
		BuildsAgree:  true,
		Profile:      settings.ProfileEvaluation,
		TimeBudget:   45 * time.Minute,
		RunID:        "abcdef",
		ProjectID:    "00000000000000000000000000000000",
		Region:       "eu-de",
		Zone:         "eu-de-01",
		ServerID:     "22222222-2222-2222-2222-222222222222",
		ServerStatus: "ACTIVE",
		VolumeType:   "SSD",
		Guest:        GuestInfo{Kernel: "6.1.0"},
		Started:      time.Unix(1_700_000_000, 0),
	}
}

func demoRecords() []catalogue.Record {
	return []catalogue.Record{
		{ID: "always-one", State: catalogue.StateDemonstrated, Elapsed: 2 * time.Second},
		{ID: "always-two", State: catalogue.StateFailed, Elapsed: time.Second},
		{ID: "costly", State: catalogue.StateNotReached, Reason: catalogue.ReasonUnauthorized},
		{
			ID:     "racy",
			State:  catalogue.StateNotReached,
			Reason: catalogue.ReasonWindow,
			Detail: "the window closed first",
		},
		{ID: "impossible", State: catalogue.StateNotReached, Reason: catalogue.ReasonNotForceable},
	}
}

// reportArgs bundles WriteReport's inputs with the demo defaults, so a
// test states only what it varies.
type reportArgs struct {
	records      []catalogue.Record
	selected     []catalogue.Check
	teardown     []error
	finalization []error
	observations []NameValue
}

func renderReport(w io.Writer, args reportArgs) error {
	if args.records == nil {
		args.records = demoRecords()
	}
	if args.selected == nil {
		args.selected = demoCatalogue
	}
	result := catalogue.Summarize(args.selected, args.records, false)
	return WriteReport(
		w,
		demoHeader(),
		args.selected,
		args.records,
		result,
		args.teardown,
		args.finalization,
		args.observations,
		time.Minute,
	)
}

func TestHeaderNamesBothBuilds(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := WriteHeader(&out, demoHeader()); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	printed := out.String()
	for _, expected := range []string{"asset build", "driver build", "v0.1.0 (abc1234)", "eu-de-01", "SSD"} {
		if !strings.Contains(printed, expected) {
			t.Errorf("WriteHeader() omits %q, got:\n%s", expected, printed)
		}
	}
}

func TestHeaderFlagsMismatchedBuilds(t *testing.T) {
	t.Parallel()

	head := demoHeader()
	head.DriverBuild = "v0.0.9 (999zzzz)"
	head.BuildsAgree = false

	var out bytes.Buffer
	if err := WriteHeader(&out, head); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if !strings.Contains(out.String(), "different sources") {
		t.Errorf("WriteHeader() = %q, want a stated build mismatch", out.String())
	}
}

func TestReportStatesOutcomesCountsAndVerdict(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := renderReport(&out, reportArgs{}); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	printed := out.String()
	for _, entry := range demoCatalogue {
		if !strings.Contains(printed, string(entry.ID)) {
			t.Errorf("WriteReport() omits check %q", entry.ID)
		}
	}
	for _, expected := range []string{
		"1 demonstrated, 1 failed, 3 not reached",
		"cost not authorized",
		"the window closed first",
		"What this run does not prove",
		"Verdict:",
		string(catalogue.VerdictFailed),
	} {
		if !strings.Contains(printed, expected) {
			t.Errorf("WriteReport() omits %q, got:\n%s", expected, printed)
		}
	}
}

func TestReportNamesAnUncataloguedRecord(t *testing.T) {
	t.Parallel()

	records := append(demoRecords(), catalogue.Record{ID: "stray", State: catalogue.StateFailed})

	var out bytes.Buffer
	if err := renderReport(&out, reportArgs{records: records}); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, "stray") {
		t.Errorf("WriteReport() omits the uncatalogued record, got:\n%s", printed)
	}
	if !strings.Contains(printed, "(uncatalogued)") {
		t.Errorf("WriteReport() does not label the stray record as uncatalogued, got:\n%s", printed)
	}
	if !strings.Contains(printed, "1 demonstrated, 2 failed, 3 not reached, of 6 checks") {
		t.Errorf("WriteReport() counts omit the stray record, got:\n%s", printed)
	}
}

func TestReportStatesReclamationOutcome(t *testing.T) {
	t.Parallel()

	var clean bytes.Buffer
	if err := renderReport(&clean, reportArgs{}); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if !strings.Contains(clean.String(), "was reclaimed") {
		t.Error("WriteReport() omits a clean reclamation")
	}

	var leaked bytes.Buffer
	failures := []error{errors.New("one volume survived teardown")}
	if err := renderReport(&leaked, reportArgs{teardown: failures}); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if !strings.Contains(leaked.String(), "survived teardown") {
		t.Error("WriteReport() omits a reclamation failure")
	}
}

func TestReportKeepsFinalizationApartFromReclamation(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	failures := []error{errors.New("close ledger: disk full")}
	if err := renderReport(&out, reportArgs{finalization: failures}); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, "was reclaimed") {
		t.Error("WriteReport() unsays a clean reclamation after a finalization failure")
	}
	if !strings.Contains(printed, "Finalization") || !strings.Contains(printed, "disk full") {
		t.Errorf("WriteReport() omits the finalization heading or error, got:\n%s", printed)
	}
}

func TestReportProofRouteDoesNotLeadWithASemicolon(t *testing.T) {
	t.Parallel()

	entry := catalogue.Check{
		ID: "impossible", Group: "deletion", ProofRoute: "offline transport script",
	}
	records := []catalogue.Record{{ID: entry.ID, State: catalogue.StateNotReached}}

	var out bytes.Buffer
	err := renderReport(&out, reportArgs{records: records, selected: []catalogue.Check{entry}})
	if err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, "offline proof route: offline transport script") {
		t.Fatalf("WriteReport() omits the proof route, got:\n%s", printed)
	}
	if strings.Contains(printed, "; offline proof route:") {
		t.Fatalf("WriteReport() prepended a semicolon to the proof route, got:\n%s", printed)
	}
}

func TestReportCarriesNoPrivateIdentifier(t *testing.T) {
	t.Parallel()

	selected := catalogue.SelectCatalogue(settings.ProfileProof)
	records := make([]catalogue.Record, len(selected))
	for i, entry := range selected {
		records[i] = catalogue.Record{ID: entry.ID, State: catalogue.StateDemonstrated}
	}

	var out bytes.Buffer
	if err := renderReport(&out, reportArgs{selected: selected, records: records}); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	var head bytes.Buffer
	if err := WriteHeader(&head, demoHeader()); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	for label, text := range map[string]string{"report": out.String(), "header": head.String()} {
		if match := privateIdentifier.FindString(text); match != "" {
			t.Errorf("the %s carries the private identifier shape %q", label, match)
		}
	}
}

func TestMaskingWriterHidesASecretSplitAcrossWrites(t *testing.T) {
	t.Parallel()

	const secret = "super-secret-access-key"

	var sink bytes.Buffer
	writer := NewMaskingWriter(&sink, ExactMask([]string{secret}))

	if _, err := writer.Write([]byte("cloud said super-secret")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := writer.Write([]byte("-access-key and stopped\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got := sink.String()
	if strings.Contains(got, secret) {
		t.Errorf("MaskingWriter leaked the credential: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("MaskingWriter removed the credential without the mask marker: %q", got)
	}
}

func TestMaskingWriterForwardsATrailingLineOnFlush(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	writer := NewMaskingWriter(&sink, ExactMask(nil))

	if _, err := writer.Write([]byte("no newline here")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sink.Len() != 0 {
		t.Error("MaskingWriter forwarded an incomplete line before Flush")
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if sink.String() != "no newline here" {
		t.Errorf("Flush() = %q, want the held line", sink.String())
	}
}

func TestProgressCountsAgainstTheCatalogue(t *testing.T) {
	t.Parallel()

	selected := catalogue.SelectCatalogue(settings.ProfileProof)
	var out bytes.Buffer
	reporter := NewProgress(&out, selected)

	entry, ok := catalogue.LookupCheck(catalogue.CheckOffsetPagingAdvances)
	if !ok {
		t.Fatalf("LookupCheck(%q) failed", catalogue.CheckOffsetPagingAdvances)
	}

	reporter.Starting(entry)
	reporter.Finished(entry, catalogue.Record{
		ID:      entry.ID,
		State:   catalogue.StateDemonstrated,
		Elapsed: 2100 * time.Millisecond,
	})

	printed := out.String()
	for _, expected := range []string{
		"running",
		"demonstrated",
		entry.Group,
		string(entry.ID),
		"2.1s",
	} {
		if !strings.Contains(printed, expected) {
			t.Errorf("Progress output omits %q, got %q", expected, printed)
		}
	}
}

func TestProgressReportsTheReasonForASkip(t *testing.T) {
	t.Parallel()

	selected := catalogue.SelectCatalogue(settings.ProfileProof)
	var out bytes.Buffer
	reporter := NewProgress(&out, selected)

	entry, ok := catalogue.LookupCheck(catalogue.CheckLaterPageRevealsCandidate)
	if !ok {
		t.Fatalf("LookupCheck(%q) failed", catalogue.CheckLaterPageRevealsCandidate)
	}
	reporter.Finished(entry, catalogue.Record{
		ID:      entry.ID,
		State:   catalogue.StateNotReached,
		Reason:  catalogue.ReasonUnauthorized,
		Elapsed: time.Second,
	})

	if !strings.Contains(out.String(), string(catalogue.ReasonUnauthorized)) {
		t.Errorf("Progress output omits the reason, got %q", out.String())
	}
}
