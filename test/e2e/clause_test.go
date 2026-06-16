//go:build e2e

package e2e

import (
	"sync"
	"testing"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
)

// skipOutcome carries the classification and explanation from an unreached
// clause back to its recorder, which is the only place that knows whether
// the clause passed or skipped.
type skipOutcome struct {
	reason catalogue.SkipReason
	detail string
}

// clauseSkips holds one skipOutcome per skipped subtest name.
var clauseSkips sync.Map

// clause runs one catalogued check and records what the run may claim
// about it. An identifier the catalogue does not name ends the scenario
// at once: that is a defect in this asset, not a finding about the cloud.
func clause(t *testing.T, id catalogue.CheckID, body func(t *testing.T)) {
	t.Helper()

	entry, known := catalogue.LookupCheck(id)
	if !known {
		t.Fatalf("check %q is not in the catalogue", id)
	}
	if !runState.selectedIDs[id] {
		return
	}

	if runState.budgetExhausted() {
		rec := catalogue.Record{
			ID:     id,
			State:  catalogue.StateNotReached,
			Reason: catalogue.ReasonBudget,
			Detail: "the run passed its own time bound before this check started",
		}
		runState.evidence.Record(rec)
		runState.progress.Finished(entry, rec)
		return
	}

	runState.progress.Starting(entry)

	var (
		skipped bool
		outcome skipOutcome
	)

	started := time.Now()
	passed := t.Run(string(id), func(st *testing.T) {
		st.Cleanup(func() {
			skipped = st.Skipped()
			if value, found := clauseSkips.LoadAndDelete(st.Name()); found {
				outcome, _ = value.(skipOutcome)
			}
		})
		body(st)
	})
	elapsed := time.Since(started)

	rec := catalogue.Record{ID: id, Elapsed: elapsed}
	switch {
	case skipped:
		rec.State = catalogue.StateNotReached
		rec.Reason = catalogue.ClassifiedReason(outcome.reason)
		rec.Detail = outcome.detail
	case !passed:
		rec.State = catalogue.StateFailed
	default:
		rec.State = catalogue.StateDemonstrated
	}

	runState.evidence.Record(rec)
	runState.progress.Finished(entry, rec)
}

// skipWith ends the running clause, classifying why a different run might reach it.
func skipWith(t *testing.T, reason catalogue.SkipReason, detail string) {
	t.Helper()
	clauseSkips.Store(t.Name(), skipOutcome{reason: reason, detail: detail})
	t.Skip(detail)
}

// notForceable ends a check whose state neither the service nor a caller
// can produce on demand.
func notForceable(t *testing.T, detail string) {
	t.Helper()
	skipWith(t, catalogue.ReasonNotForceable, detail)
}

// notAuthorized ends a check whose resource cost the operator has not authorized.
func notAuthorized(t *testing.T, detail string) {
	t.Helper()
	skipWith(t, catalogue.ReasonUnauthorized, detail)
}

// windowMissed ends a check whose transient state the run did not catch.
func windowMissed(t *testing.T, detail string) {
	t.Helper()
	skipWith(t, catalogue.ReasonWindow, detail)
}

// shapeLimited ends a check this instance cannot exhibit.
func shapeLimited(t *testing.T, detail string) {
	t.Helper()
	skipWith(t, catalogue.ReasonShape, detail)
}

// blocked ends a check whose precondition an earlier check failed to establish, so it stays in the
// record as unreached.
func blocked(t *testing.T, detail string) {
	t.Helper()
	skipWith(t, catalogue.ReasonBlocked, detail)
}
