package report

import (
	"strings"
	"testing"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
)

func TestEvidenceThroughTheExactMaskCarriesNoKnownSecret(t *testing.T) {
	t.Parallel()

	const secret = "fake-secret-value-1234567890"
	var sink strings.Builder
	masked := NewMaskingWriter(&sink, ExactMask([]string{secret}))

	ev := NewEvidence(ReportHeader{RunID: "run-1", ProjectID: "project-1"})
	ev.DescribeGuest(GuestInfo{Kernel: "0.0.0-test"})
	ev.Observe("cloud_error_detail", "the service answered with "+secret+" embedded")
	ev.Record(catalogue.Record{ID: "demo.check", State: catalogue.StateFailed})

	err := ev.Render(
		masked,
		[]catalogue.Check{{ID: "demo.check"}},
		catalogue.Summary{Total: 1, Failed: 1},
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err = masked.Flush(); err != nil {
		t.Fatalf("Flush() = %v", err)
	}

	rendered := sink.String()
	if rendered == "" {
		t.Fatal("Render wrote nothing through the masked writer")
	}
	if strings.Contains(rendered, secret) {
		t.Errorf("the rendered record carries the known secret value:\n%s", rendered)
	}
	if !strings.Contains(rendered, "cloud_error_detail") {
		t.Errorf("the rendered record lost the observation that carried the secret:\n%s", rendered)
	}
}

func TestEvidenceSchemaNamesTheContractFields(t *testing.T) {
	t.Parallel()

	var sink strings.Builder
	ev := NewEvidence(demoHeader())
	ev.DescribeGuest(GuestInfo{Kernel: "6.1.0"})
	ev.Observe("server_id", "abc")
	ev.Record(catalogue.Record{
		ID:     "always-one",
		State:  catalogue.StateNotReached,
		Reason: catalogue.ReasonUnauthorized,
	})

	err := ev.Render(
		&sink,
		demoCatalogue[:1],
		catalogue.Summary{Total: 1, NotReached: 1, Verdict: catalogue.VerdictDemonstrated},
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	rendered := sink.String()
	for _, expected := range []string{
		"run_id",
		"approved_project_id",
		"End-to-end run evidence",
		"not_reached_because",
		"clauses",
		"observations",
		"cost not authorized",
		"abcdef",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("Render() omits %q, got:\n%s", expected, rendered)
		}
	}
}
