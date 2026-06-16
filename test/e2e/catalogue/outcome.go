package catalogue

import (
	"fmt"
	"time"
)

// State records what a run may claim about one check. A check an
// execution never triggered is never reported as demonstrated.
type State string

const (
	// StateDemonstrated means the run forced the condition and the driver
	// behaved as the check requires.
	StateDemonstrated State = "demonstrated"
	// StateNotReached means the run did not trigger the check.
	StateNotReached State = "not reached"
	// StateFailed means the run triggered the check and the driver did not
	// behave as required.
	StateFailed State = "failed"
)

// SkipReason classifies why a check was not reached, which is what decides
// whether another run could reach it. It is a second axis, not more
// states: the three states above are the vocabulary the evidence record
// already uses.
type SkipReason string

const (
	// ReasonNone is an empty skip reason. ClassifiedReason names it as unclassified.
	ReasonNone SkipReason = ""
	// ReasonUnauthorized names a cost the operator did not authorize.
	ReasonUnauthorized SkipReason = "cost not authorized"
	// ReasonWindow names a transient state the run did not catch.
	ReasonWindow SkipReason = "timing window missed"
	// ReasonShape names a property this instance does not exhibit.
	ReasonShape SkipReason = "instance shape"
	// ReasonNotForceable names a state no run of this asset can produce.
	ReasonNotForceable SkipReason = "not forceable here"
	// ReasonBlocked names a precondition an earlier check failed to establish.
	ReasonBlocked SkipReason = "blocked by an earlier failure"
	// ReasonBudget names the run's own wall-clock bound.
	ReasonBudget SkipReason = "time budget exhausted"
	// ReasonNeverRan names a catalogued check no execution reached, which
	// reconciliation discovers.
	ReasonNeverRan SkipReason = "never executed"
	// ReasonUnclassified names a skip that stated no reason, which is a
	// defect in this asset: it is flagged on every tier and never excused
	// from strict coverage.
	ReasonUnclassified SkipReason = "unclassified skip"
)

// ClassifiedReason names a skip that gave no reason as unclassified.
// A skip that already classified itself keeps that reason.
func ClassifiedReason(reason SkipReason) SkipReason {
	if reason == ReasonNone {
		return ReasonUnclassified
	}
	return reason
}

// Record is what a run may claim about one catalogued check.
type Record struct {
	// ID is the catalogued check this record describes.
	ID CheckID
	// State is demonstrated, failed or not reached.
	State State
	// Reason classifies a skip. It is empty on a demonstrated or failed check.
	Reason SkipReason
	// Detail is an optional operator-visible phrase that prefers over Reason in a report.
	Detail string
	// Elapsed is how long the check ran. Zero means it was never timed.
	Elapsed time.Duration
}

// Verdict is the one-line answer a reader takes away from a run.
type Verdict string

const (
	// VerdictDemonstrated reports that every check the run could force was demonstrated.
	VerdictDemonstrated Verdict = "every check this run could force was demonstrated"
	// VerdictFailed reports that at least one check failed or the catalogue and the run disagree.
	VerdictFailed Verdict = "at least one check failed"
	// VerdictIncomplete reports that the run finished without covering everything it selected.
	VerdictIncomplete Verdict = "the run did not reach every check it selected"
	// VerdictRefused reports input or an environment the run will not start on.
	// Summarize never returns it; the harness uses it for preflight refusal.
	VerdictRefused Verdict = "the run refused to start"
)

// Process exit codes. They are distinct so a caller can tell a driver
// defect from a misconfiguration and from a run that simply ran out of
// time.
const (
	// ExitDemonstrated reports that every check the run could force was demonstrated.
	ExitDemonstrated = 0
	// ExitFailed reports a failed check, a teardown failure or a resource that survived.
	ExitFailed = 1
	// ExitRefused reports input or an environment the run will not start on.
	ExitRefused = 2
	// ExitIncomplete reports a run that reached no failure but did not cover everything it selected.
	ExitIncomplete = 3
)

// Summary is the counted outcome of one run and the verdict it produced.
type Summary struct {
	// Verdict is the one-line answer a reader takes away.
	Verdict Verdict
	// Demonstrated is how many records ended demonstrated.
	Demonstrated int
	// NotReached is how many records ended not reached.
	NotReached int
	// Failed is how many records ended failed.
	Failed int
	// Total is the number of records counted.
	Total int
	// Strict is the coverage mode the caller asked Summarize to apply.
	Strict bool
	// UnclassifiedSkips is how many not-reached records stated no classified reason.
	UnclassifiedSkips int
	// MissingForceable is how many selected forceable checks were recorded as not reached.
	MissingForceable int
	// Records is a copy of the records that were counted.
	Records []Record
}

// Reconcile completes recorded with one entry per selected check no execution reached, in catalogue
// order, so a check that never ran still appears in the record. Duplicate records for one check
// keep the worst outcome. A record naming no selected check is appended, not dropped.
func Reconcile(selected []Check, recorded []Record) []Record {
	seen := make(map[CheckID]Record, len(recorded))
	for _, rec := range recorded {
		existing, found := seen[rec.ID]
		if found && stateSeverity(existing.State) >= stateSeverity(rec.State) {
			continue
		}
		seen[rec.ID] = rec
	}

	selectedIDs := make(map[CheckID]bool, len(selected))
	complete := make([]Record, 0, len(selected)+len(recorded))
	for _, entry := range selected {
		selectedIDs[entry.ID] = true
		if rec, found := seen[entry.ID]; found {
			complete = append(complete, rec)
			continue
		}
		complete = append(complete, Record{
			ID:     entry.ID,
			State:  StateNotReached,
			Reason: ReasonNeverRan,
		})
	}

	appended := make(map[CheckID]bool)
	for _, rec := range recorded {
		if selectedIDs[rec.ID] || appended[rec.ID] {
			continue
		}
		appended[rec.ID] = true
		complete = append(complete, seen[rec.ID])
	}
	return complete
}

// stateSeverity orders states so reconciliation can keep the worst
// of two records for one check: a failure outranks a skip. A skip
// outranks a demonstration.
func stateSeverity(state State) int {
	switch state {
	case StateFailed:
		return 3
	case StateNotReached:
		return 2
	case StateDemonstrated:
		return 1
	default:
		return 0
	}
}

// Summarize counts outcomes, checks each one against its catalogued
// reachability and decides the run's verdict. Callers reconcile first
// when silence should become a recorded skip.
func Summarize(selected []Check, records []Record, strict bool) Summary {
	result := Summary{
		Total:   len(records),
		Strict:  strict,
		Records: append([]Record(nil), records...),
	}
	byID := make(map[CheckID]Record, len(records))
	byReason := map[SkipReason]int{}

	for _, rec := range records {
		byID[rec.ID] = rec
		switch rec.State {
		case StateDemonstrated:
			result.Demonstrated++
		case StateFailed:
			result.Failed++
		case StateNotReached:
			result.NotReached++
			byReason[rec.Reason]++
			if ClassifiedReason(rec.Reason) == ReasonUnclassified {
				result.UnclassifiedSkips++
			}
		}
	}

	incoherent := false
	for _, entry := range selected {
		rec, found := byID[entry.ID]
		if !found {
			continue
		}
		if entry.Tier == TierForceable && rec.State == StateNotReached {
			result.MissingForceable++
		}
		if len(incoherence(entry, rec)) > 0 {
			incoherent = true
		}
	}

	result.Verdict = decide(result, byReason, incoherent)
	return result
}

// ExitCode maps the summary's verdict onto the process exit the harness
// should use. An unrecognized verdict is treated as a failure.
func (s Summary) ExitCode() int {
	switch s.Verdict {
	case VerdictDemonstrated:
		return ExitDemonstrated
	case VerdictFailed:
		return ExitFailed
	case VerdictRefused:
		return ExitRefused
	case VerdictIncomplete:
		return ExitIncomplete
	default:
		return ExitFailed
	}
}

// incoherence reports where a check's outcome disagrees with what the
// catalogue says it can reach. A disagreement is a finding about the
// catalogue, not about the cloud.
func incoherence(entry Check, rec Record) []string {
	switch {
	case rec.State == StateNotReached && rec.Reason == ReasonUnclassified:
		return []string{
			fmt.Sprintf(
				"%s was skipped without classifying why, so nothing says whether another run could reach it",
				entry.ID,
			),
		}
	case rec.State == StateNotReached && rec.Reason == ReasonNeverRan:
		return []string{
			fmt.Sprintf("%s was selected but no check executed it", entry.ID),
		}
	case entry.Tier == TierNotForceable && rec.State == StateDemonstrated:
		return []string{
			fmt.Sprintf(
				"%s is catalogued as impossible to force but this run demonstrated it, so the catalogue is"+
					" stale",
				entry.ID,
			),
		}
	case rec.State == StateNotReached && !skipExplainsTier(entry.Tier, rec.Reason):
		return []string{skipFinding(entry, rec.Reason)}
	}
	return nil
}

// skipExplainsTier reports whether a skip reason coherently explains not
// reaching a check of the given tier. An exhausted budget, an earlier
// failure and a check that never ran can befall any tier; beyond those,
// a tier excuses only the reasons its own definition names.
func skipExplainsTier(tier CheckTier, reason SkipReason) bool {
	switch reason {
	case ReasonBudget, ReasonBlocked, ReasonNeverRan:
		return true
	case ReasonUnauthorized:
		return tier == TierAuthorized
	case ReasonWindow, ReasonShape:
		return tier == TierOpportunistic
	case ReasonNotForceable:
		return tier == TierNotForceable
	default:
		return false
	}
}

// skipFinding names the disagreement in the tier's own terms, so the
// reader learns what the catalogue claims about the check the skip
// contradicts.
func skipFinding(entry Check, reason SkipReason) string {
	switch entry.Tier {
	case TierForceable:
		return fmt.Sprintf(
			"%s should be reachable by any run but was skipped as %q",
			entry.ID,
			reason,
		)
	case TierAuthorized:
		return fmt.Sprintf(
			"%s needs its cost authorized, so a skip as %q does not explain it",
			entry.ID,
			reason,
		)
	case TierOpportunistic:
		return fmt.Sprintf(
			"%s waits on a timing window or the instance shape, so a skip as %q does not explain it",
			entry.ID,
			reason,
		)
	default:
		return fmt.Sprintf(
			"%s is catalogued as impossible to force but was skipped as %q",
			entry.ID,
			reason,
		)
	}
}

// decide turns counted outcomes into one verdict. Checks no run can force are left out of the
// coverage count, so a strict run can still pass.
func decide(counted Summary, byReason map[SkipReason]int, incoherent bool) Verdict {
	switch {
	case counted.Failed > 0:
		return VerdictFailed
	case incoherent:
		return VerdictFailed
	case byReason[ReasonBudget] > 0:
		return VerdictIncomplete
	case counted.Strict && shortCoverage(counted, byReason) > 0:
		return VerdictIncomplete
	default:
		return VerdictDemonstrated
	}
}

// shortCoverage counts the checks a strict run required and did not reach.
// A check no run can force never counts. Neither does a transient
// state a run cannot command.
func shortCoverage(counted Summary, byReason map[SkipReason]int) int {
	short := counted.NotReached
	short -= byReason[ReasonNotForceable]
	short -= byReason[ReasonWindow]
	short -= byReason[ReasonShape]
	if short < 0 {
		return 0
	}
	return short
}
