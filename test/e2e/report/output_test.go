package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

// stageArgs bundles StageFinalOutputs's inputs with the demo defaults, so
// a test states only what it varies.
type stageArgs struct {
	records  []catalogue.Record
	selected []catalogue.Check
	evidence *Evidence
	contain  func(string, ...string) error
}

func stageOutputs(args stageArgs) (FinalOutputs, error) {
	if args.evidence == nil {
		args.evidence = NewEvidence(ReportHeader{RunID: "run-1", ProjectID: "project-1"})
	}
	if args.contain == nil {
		args.contain = func(string, ...string) error { return nil }
	}
	result := catalogue.Summarize(args.selected, args.records, false)
	return StageFinalOutputs(
		ExactMask(nil),
		demoHeader(),
		args.records,
		args.selected,
		result,
		nil,
		nil,
		nil,
		time.Second,
		args.evidence,
		args.contain,
	)
}

func TestFinalOutputsUseDistinctReadableAndJSONDestinations(t *testing.T) {
	t.Parallel()

	set := &settings.Settings{Profile: settings.ProfileProof}
	destinations, err := ResolveOutputDestinations(set, "run-1")
	if err != nil {
		t.Fatalf("ResolveOutputDestinations: %v", err)
	}
	if filepath.Base(destinations.EvidencePath) != "conformance-run-1.json" {
		t.Fatalf("default evidence path = %q", destinations.EvidencePath)
	}
	destinations.EvidencePath = filepath.Join(t.TempDir(), filepath.Base(destinations.EvidencePath))

	records := []catalogue.Record{{ID: "always-one", State: catalogue.StateDemonstrated}}
	outputs, err := stageOutputs(stageArgs{records: records, selected: demoCatalogue[:1]})
	if err != nil {
		t.Fatalf("StageFinalOutputs: %v", err)
	}

	var report bytes.Buffer
	if err = CommitFinalOutputs(destinations, outputs, &report, OpenOutput); err != nil {
		t.Fatalf("CommitFinalOutputs: %v", err)
	}
	evidenceBytes, err := os.ReadFile(destinations.EvidencePath)
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(evidenceBytes))
	var record map[string]any
	if err = decoder.Decode(&record); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	if err = decoder.Decode(&record); !errors.Is(err, io.EOF) {
		t.Fatalf("evidence contains more than one JSON object: %v", err)
	}
	if strings.Contains(report.String(), "End-to-end run evidence") {
		t.Fatal("the readable report contains the evidence record")
	}
}

func TestFinalOutputContainmentReceivesBothCompleteArtifacts(t *testing.T) {
	t.Parallel()

	records := []catalogue.Record{{
		ID: "always-one", State: catalogue.StateFailed, Detail: "report-marker",
	}}
	ev := NewEvidence(ReportHeader{RunID: "run-1", ProjectID: "project-1"})
	ev.Observe("evidence_marker", "evidence-marker")
	seen := false
	_, err := stageOutputs(stageArgs{
		records:  records,
		selected: demoCatalogue[:1],
		evidence: ev,
		contain: func(_ string, content ...string) error {
			seen = len(content) == 2 && strings.Contains(content[0], "report-marker") &&
				strings.Contains(content[1], "evidence-marker")
			return errors.New("containment refusal")
		},
	})
	if err == nil || !seen {
		t.Fatalf("StageFinalOutputs() = %v, complete artifacts seen = %v", err, seen)
	}
}

func TestExactMaskReplacesOverlappingSecretsLongestFirst(t *testing.T) {
	t.Parallel()

	masked := ExactMask([]string{"secret", "secret-with-suffix"})("secret-with-suffix")
	if strings.Contains(masked, "secret") || strings.Contains(masked, "-with-suffix") {
		t.Fatalf("ExactMask overlapping credential survived as %q", masked)
	}
}

type failedOutput struct {
	writeErr error
	syncErr  error
	closeErr error
	short    bool
}

func (f *failedOutput) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.short && len(p) > 0 {
		return len(p) - 1, nil
	}
	return len(p), nil
}

func (f *failedOutput) Sync() error  { return f.syncErr }
func (f *failedOutput) Close() error { return f.closeErr }

func TestRequiredOutputFailuresAreReturned(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("output failure")
	cases := []struct {
		name string
		file *failedOutput
	}{
		{name: "write", file: &failedOutput{writeErr: wantErr}},
		{name: "short write", file: &failedOutput{short: true}},
		{name: "sync", file: &failedOutput{syncErr: wantErr}},
		{name: "close", file: &failedOutput{closeErr: wantErr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CommitFinalOutputs(
				OutputDestinations{EvidencePath: "evidence.json"},
				FinalOutputs{Report: []byte("report"), Evidence: []byte("evidence")},
				io.Discard,
				func(string) (io.WriteCloser, error) { return tc.file, nil },
			)
			if err == nil {
				t.Fatal("CommitFinalOutputs() succeeded despite required output failure")
			}
		})
	}
}

func TestRenderAndMaskFailuresAreReturned(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("write failed")
	failing := &failAfterWriter{err: wantErr}
	if err := WriteHeader(failing, demoHeader()); !errors.Is(err, wantErr) {
		t.Errorf("WriteHeader() = %v, want writer failure", err)
	}
	if err := renderReport(failing, reportArgs{}); !errors.Is(err, wantErr) {
		t.Errorf("WriteReport() = %v, want writer failure", err)
	}
	ev := NewEvidence(ReportHeader{RunID: "run-1", ProjectID: "project-1"})
	if err := ev.Render(failing, demoCatalogue, catalogue.Summary{}); !errors.Is(err, wantErr) {
		t.Errorf("Evidence.Render() = %v, want handler failure", err)
	}
	_, err := renderMasked(func(line string) string { return line }, func(w io.Writer) error {
		masked, ok := w.(*MaskingWriter)
		if !ok {
			t.Fatalf("renderer received %T, want *MaskingWriter", w)
		}
		masked.next = failing
		_, writeErr := w.Write([]byte("trailing line"))
		return writeErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("renderMasked() = %v, want masking flush failure", err)
	}
}

type failAfterWriter struct {
	remaining int
	err       error
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	if len(p) > w.remaining {
		written := w.remaining
		w.remaining = 0
		return written, w.err
	}
	w.remaining -= len(p)
	return len(p), nil
}

func TestReportReturnsFailuresThroughoutRendering(t *testing.T) {
	t.Parallel()

	observations := []NameValue{{Name: "observed", Value: "value"}}
	var complete bytes.Buffer
	if err := renderReport(&complete, reportArgs{observations: observations}); err != nil {
		t.Fatalf("render complete report: %v", err)
	}
	wantErr := errors.New("staged write failed")
	for _, limit := range []int{0, complete.Len() / 4, complete.Len() / 2, complete.Len() - 1} {
		writer := &failAfterWriter{remaining: limit, err: wantErr}
		err := renderReport(writer, reportArgs{observations: observations})
		if !errors.Is(err, wantErr) {
			t.Errorf("WriteReport() with limit %d = %v, want staged failure", limit, err)
		}
	}
}

func TestExplicitOutputPathsMustDiffer(t *testing.T) {
	t.Parallel()

	set := &settings.Settings{ReportPath: "result", EvidencePath: "./result"}
	if _, err := ResolveOutputDestinations(set, "run-1"); err == nil {
		t.Fatal("equal report and evidence paths were accepted")
	}
}

func TestOutputPathsAliasedThroughASymlinkAreRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(target, []byte("existing"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link-to-out.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	set := &settings.Settings{ReportPath: target, EvidencePath: link}
	if _, err := ResolveOutputDestinations(set, "run-1"); err == nil {
		t.Fatal("a symlink alias of the report path was accepted as the evidence path")
	}
}

func TestDanglingSymlinkDestinationIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "absent"), dangling); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	set := &settings.Settings{ReportPath: filepath.Join(dir, "report.txt"), EvidencePath: dangling}
	if _, err := ResolveOutputDestinations(set, "run-1"); err == nil {
		t.Fatal("a dangling symlink was accepted as a destination")
	}
}

func TestSelectedCatalogueControlsEveryFinalSurface(t *testing.T) {
	t.Parallel()

	selected := catalogue.SelectCatalogue(settings.ProfileEvaluation)
	records := make([]catalogue.Record, len(selected))
	for i, entry := range selected {
		records[i] = catalogue.Record{ID: entry.ID, State: catalogue.StateDemonstrated}
	}
	ev := NewEvidence(ReportHeader{RunID: "run-1", ProjectID: "project-1"})
	ev.Replace(records)
	outputs, err := stageOutputs(stageArgs{records: records, selected: selected, evidence: ev})
	if err != nil {
		t.Fatalf("StageFinalOutputs: %v", err)
	}
	all := catalogue.SelectCatalogue(settings.ProfileProof)
	selectedSet := make(map[catalogue.CheckID]bool, len(selected))
	for _, entry := range selected {
		selectedSet[entry.ID] = true
	}
	for _, entry := range all {
		present := bytes.Contains(outputs.Report, []byte(entry.ID)) ||
			bytes.Contains(outputs.Evidence, []byte(entry.ID))
		if present != selectedSet[entry.ID] {
			t.Errorf(
				"check %q present = %v, selected = %v",
				entry.ID,
				present,
				selectedSet[entry.ID],
			)
		}
	}

	var progressOut bytes.Buffer
	progress := NewProgress(&progressOut, selected)
	progress.Starting(selected[len(selected)-1])
	if !strings.Contains(progressOut.String(), "/"+strconv.Itoa(len(selected))+"]") {
		t.Errorf("selected progress total missing from %q", progressOut.String())
	}
}

func TestNotForceableProofRouteAppearsInReportAndEvidence(t *testing.T) {
	t.Parallel()

	entry, ok := catalogue.LookupCheck(catalogue.CheckFailedDeletionReported)
	if !ok {
		t.Fatal("not-forceable check is absent")
	}
	records := []catalogue.Record{{
		ID: entry.ID, State: catalogue.StateNotReached, Reason: catalogue.ReasonNotForceable,
	}}
	ev := NewEvidence(ReportHeader{RunID: "run-1", ProjectID: "project-1"})
	ev.Replace(records)
	outputs, err := stageOutputs(
		stageArgs{records: records, selected: []catalogue.Check{entry}, evidence: ev},
	)
	if err != nil {
		t.Fatalf("StageFinalOutputs: %v", err)
	}
	if !bytes.Contains(outputs.Report, []byte(entry.ProofRoute)) {
		t.Error("readable report omits the offline proof route")
	}
	if !bytes.Contains(outputs.Evidence, []byte(entry.ProofRoute)) {
		t.Error("evidence omits the offline proof route")
	}
}
