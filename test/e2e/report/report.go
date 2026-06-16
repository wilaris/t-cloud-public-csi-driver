// Package report prints live progress, the readable end-of-run report,
// and the machine-readable evidence record.
//
// A run writes progress as checks start and finish so a wait on the cloud looks different from a
// stop. After the run, the readable report and the JSON evidence are rendered in memory, masked and
// then committed to their destinations. This package does not call the cloud.
package report

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	driverlog "git.wilaris.dev/t-cloud-public-csi-driver/internal/log"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

// NameValue is one labelled fact in the report header or the observed-values list.
type NameValue struct {
	// Name is the label printed in the first column.
	Name string
	// Value is the fact printed beside Name.
	Value string
}

// RedactLine masks the credential shapes the driver's log conventions
// recognize. It is safe to call before a run has loaded its own credentials.
func RedactLine(s string) string {
	return driverlog.RedactString(s)
}

// MaskingWriter forwards complete lines after applying a mask, so a
// secret split across Write calls cannot reach the destination.
type MaskingWriter struct {
	mu      sync.Mutex
	next    io.Writer
	pending []byte
	mask    func(string) string
}

// NewMaskingWriter forwards to next with mask applied to each complete line.
// A nil mask is treated as the identity function.
func NewMaskingWriter(next io.Writer, mask func(string) string) *MaskingWriter {
	if mask == nil {
		mask = func(s string) string { return s }
	}
	return &MaskingWriter{next: next, mask: mask}
}

// Write holds bytes until a newline so the mask can see each line whole.
func (w *MaskingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending = append(w.pending, p...)
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			return len(p), nil
		}
		line := string(w.pending[:index+1])
		w.pending = w.pending[index+1:]
		if _, err := io.WriteString(w.next, w.mask(line)); err != nil {
			return len(p), err
		}
	}
}

// Flush forwards a trailing line that carried no newline.
func (w *MaskingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.pending) == 0 {
		return nil
	}
	line := string(w.pending)
	w.pending = nil
	_, err := io.WriteString(w.next, w.mask(line))
	return err
}

// ExactMask replaces each known secret value, then applies the pattern rules. The run already holds
// the credential strings, so those are replaced first.
func ExactMask(secrets []string) func(string) string {
	ordered := append([]string(nil), secrets...)
	ordered = slices.DeleteFunc(ordered, func(secret string) bool { return secret == "" })
	slices.SortFunc(ordered, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return func(line string) string {
		for _, secret := range ordered {
			line = strings.ReplaceAll(line, secret, "***")
		}
		return driverlog.RedactString(line)
	}
}

// Progress reports each check as it starts and as it ends. It writes
// to its own stream so the staged report stays clean.
type Progress struct {
	mu        sync.Mutex
	w         io.Writer
	total     int
	positions map[catalogue.CheckID]int
}

// NewProgress reports positions against cat. The counter is the check's
// place in that list, not a running tally, so it stays meaningful when
// a run selects a subset.
func NewProgress(w io.Writer, cat []catalogue.Check) *Progress {
	return &Progress{w: w, total: len(cat), positions: positionIndex(cat)}
}

func positionIndex(cat []catalogue.Check) map[catalogue.CheckID]int {
	positions := make(map[catalogue.CheckID]int, len(cat))
	for i, entry := range cat {
		positions[entry.ID] = i + 1
	}
	return positions
}

// Starting prints that a check has begun.
func (p *Progress) Starting(entry catalogue.Check) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.w, "%s running       %s / %s\n", p.counter(entry), entry.Group, entry.ID)
}

// Finished prints the check's outcome. A skip prints its reason, not its duration.
func (p *Progress) Finished(entry catalogue.Check, rec catalogue.Record) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	trailer := rec.Elapsed.Round(time.Millisecond).String()
	if rec.State == catalogue.StateNotReached {
		trailer = string(rec.Reason)
	}
	fmt.Fprintf(
		p.w,
		"%s %-13s %s / %s   %s\n",
		p.counter(entry),
		rec.State,
		entry.Group,
		entry.ID,
		trailer,
	)
}

func (p *Progress) counter(entry catalogue.Check) string {
	return fmt.Sprintf("[%2d/%d]", p.positions[entry.ID], p.total)
}

// ReportHeader is the identity of one run, reduced to text so the
// report can be rendered and checked without any cloud type.
type ReportHeader struct {
	// Tool is the asset's own name. It is the first line of the header.
	Tool string
	// ToolBuild is the stamped identity of the conformance asset.
	ToolBuild string
	// DriverBinary is the path of the driver the run started.
	DriverBinary string
	// DriverBuild is the stamped identity the driver reported.
	DriverBuild string
	// BuildsAgree is false when the asset and the driver came from different sources.
	BuildsAgree bool
	// Profile is the audience the run selected.
	Profile settings.Profile
	// Strict is the coverage mode the run applied.
	Strict bool
	// TimeBudget is the wall-clock bound the run accepted.
	TimeBudget time.Duration
	// RunID is the short identifier that names this run's artifacts.
	RunID string
	// ProjectID is the project the operator asserted may be mutated.
	ProjectID string
	// Region is the cloud region the instance sits in.
	Region string
	// Zone is the availability zone the instance sits in.
	Zone string
	// ServerID is the instance the run executed on.
	ServerID string
	// ServerStatus is the instance state observed at preflight.
	ServerStatus string
	// VolumeType is the regional volume type the run provisioned.
	VolumeType string
	// Guest names the host facilities the run exercised.
	Guest GuestInfo
	// Started is when the run began.
	Started time.Time
}

// WriteHeader prints the run identity before the results.
func WriteHeader(w io.Writer, h ReportHeader) error {
	facts := []NameValue{
		{"asset build", h.ToolBuild},
		{"driver binary", h.DriverBinary},
		{"driver build", h.DriverBuild},
		{"profile", string(h.Profile)},
		{"strict coverage", fmt.Sprint(h.Strict)},
		{"time budget", h.TimeBudget.String()},
		{"run identifier", h.RunID},
		{"approved project", h.ProjectID},
		{"region", h.Region},
		{"availability zone", h.Zone},
		{"instance", h.ServerID},
		{"instance state", h.ServerStatus},
		{"volume type", h.VolumeType},
		{"started", h.Started.UTC().Format(time.RFC3339)},
	}
	for _, fact := range guestFacts(h.Guest) {
		if fact.Value == "" {
			continue
		}
		facts = append(facts, fact)
	}

	if _, err := fmt.Fprintf(w, "%s\n\n", h.Tool); err != nil {
		return err
	}
	tab := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, fact := range facts {
		if _, err := fmt.Fprintf(tab, "  %s\t%s\n", fact.Name, fact.Value); err != nil {
			return err
		}
	}
	if err := tab.Flush(); err != nil {
		return err
	}

	if !h.BuildsAgree {
		_, err := fmt.Fprintf(
			w,
			"\n  The asset and the driver were built from different sources. A verdict from this run\n"+
				"  describes neither build on its own.\n",
		)
		if err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func guestFacts(g GuestInfo) []NameValue {
	return []NameValue{
		{"guest kernel", g.Kernel},
		{"guest udev", g.Udev},
		{"guest blkid", g.Blkid},
		{"guest mkfs.ext4", g.MkfsExt4},
		{"guest mkfs.xfs", g.MkfsXfs},
	}
}

// nonClaims lists the limits of one run, printed under the report heading.
var nonClaims = []string{
	"It used this instance's own kernel, udev and filesystem utilities, whose versions are named above. It did not use the versions inside the image a release ships.",
	"It covered one availability zone, one volume type and one instance shape. It says nothing about any other.",
	"It drove the driver's own service surface directly. No orchestrator, node agent or sidecar took part.",
	"A check reported as not forceable names a state neither the service nor a caller can produce on demand. This run does not cover that state.",
	"It says nothing about capacity, quota, throughput or latency.",
	"A demonstrated check is an observation about these builds in this project at this time.",
}

// WriteReport renders the end-of-run report: what ran, what it demonstrated and what it does not
// prove.
func WriteReport(
	w io.Writer,
	h ReportHeader,
	selected []catalogue.Check,
	records []catalogue.Record,
	result catalogue.Summary,
	teardown []error,
	finalization []error,
	observations []NameValue,
	elapsed time.Duration,
) error {
	byID := make(map[catalogue.CheckID]catalogue.Record, len(records))
	byReason := map[catalogue.SkipReason]int{}
	for _, rec := range records {
		byID[rec.ID] = rec
		if rec.State == catalogue.StateNotReached {
			byReason[rec.Reason]++
		}
	}

	if _, err := fmt.Fprintf(w, "Checks\n\n"); err != nil {
		return err
	}
	tab := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tab, "  #\tSCENARIO\tCHECK\tOUTCOME\tELAPSED\tWHY"); err != nil {
		return err
	}
	catalogued := make(map[catalogue.CheckID]bool, len(selected))
	for _, entry := range selected {
		catalogued[entry.ID] = true
	}

	row := func(position int, entry catalogue.Check, rec catalogue.Record) error {
		took := ""
		if rec.Elapsed > 0 {
			took = rec.Elapsed.Round(time.Millisecond).String()
		}
		why := reasonText(rec)
		if entry.ProofRoute != "" {
			route := "offline proof route: " + entry.ProofRoute
			if why == "" {
				why = route
			} else {
				why = why + "; " + route
			}
		}
		_, err := fmt.Fprintf(
			tab,
			"  %d\t%s\t%s\t%s\t%s\t%s\n",
			position,
			entry.Group,
			rec.ID,
			rec.State,
			took,
			why,
		)
		return err
	}

	for i, entry := range selected {
		rec, found := byID[entry.ID]
		if !found {
			continue
		}
		if err := row(i+1, entry, rec); err != nil {
			return err
		}
	}
	// A record naming no selected check still counts in the summary, so
	// it gets a row too; otherwise the counts and the table would disagree.
	position := len(selected)
	for _, rec := range records {
		if catalogued[rec.ID] {
			continue
		}
		position++
		uncatalogued := catalogue.Check{Group: "(uncatalogued)"}
		if err := row(position, uncatalogued, rec); err != nil {
			return err
		}
	}
	if err := tab.Flush(); err != nil {
		return err
	}

	_, err := fmt.Fprintf(
		w,
		"\n  %d demonstrated, %d failed, %d not reached, of %d checks in %s.\n",
		result.Demonstrated,
		result.Failed,
		result.NotReached,
		result.Total,
		elapsed.Round(time.Second),
	)
	if err != nil {
		return err
	}
	for _, reason := range orderedReasons(byReason) {
		_, err = fmt.Fprintf(
			w,
			"    %d not reached: %s\n",
			byReason[reason],
			reason,
		)
		if err != nil {
			return err
		}
	}

	findings := coherenceFindings(selected, records)
	if len(findings) > 0 {
		if _, err = fmt.Fprintf(w, "\nFindings about this asset\n\n"); err != nil {
			return err
		}
		for _, finding := range findings {
			if _, err = fmt.Fprintf(w, "  %s\n", finding); err != nil {
				return err
			}
		}
	}

	if _, err = fmt.Fprintf(w, "\nReclamation\n\n"); err != nil {
		return err
	}
	if len(teardown) == 0 {
		if _, err = fmt.Fprintf(w, "  Everything this run created was reclaimed.\n"); err != nil {
			return err
		}
	}
	for _, failure := range teardown {
		if _, err = fmt.Fprintf(w, "  %s\n", RedactLine(failure.Error())); err != nil {
			return err
		}
	}

	// A failure finalizing the run's own records is not a surviving
	// resource, so it never appears under the reclamation heading.
	if len(finalization) > 0 {
		if _, err = fmt.Fprintf(w, "\nFinalization\n\n"); err != nil {
			return err
		}
		for _, failure := range finalization {
			if _, err = fmt.Fprintf(w, "  %s\n", RedactLine(failure.Error())); err != nil {
				return err
			}
		}
	}

	if len(observations) > 0 {
		if _, err = fmt.Fprintf(w, "\nObserved values\n\n"); err != nil {
			return err
		}
		obs := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, fact := range observations {
			if _, err = fmt.Fprintf(obs, "  %s\t%s\n", fact.Name, fact.Value); err != nil {
				return err
			}
		}
		if err = obs.Flush(); err != nil {
			return err
		}
	}

	if _, err = fmt.Fprintf(w, "\nWhat this run does not prove\n\n"); err != nil {
		return err
	}
	for _, claim := range nonClaims {
		if _, err = fmt.Fprintf(w, "  %s\n", wrapIndent(claim, 94, "  ")); err != nil {
			return err
		}
	}

	// The verdict repeats what produced it, so the tail of a long
	// report stands on its own.
	_, err = fmt.Fprintf(
		w,
		"\nVerdict: %s (exit %d).\n  Run %s under the %s profile, driving %s.\n",
		result.Verdict,
		result.ExitCode(),
		h.RunID,
		h.Profile,
		h.DriverBuild,
	)
	if err != nil {
		return err
	}
	if !h.BuildsAgree {
		_, err = fmt.Fprintf(
			w,
			"  The asset build %s does not match, so this verdict describes neither build alone.\n",
			h.ToolBuild,
		)
		return err
	}
	return nil
}

// reasonText explains a record in one phrase, preferring the detail the check supplied.
func reasonText(rec catalogue.Record) string {
	switch {
	case rec.Detail != "":
		return RedactLine(rec.Detail)
	case rec.Reason != catalogue.ReasonNone:
		return string(rec.Reason)
	default:
		return ""
	}
}

// orderedReasons lists the reasons present, in the fixed order a reader can rely on.
func orderedReasons(counts map[catalogue.SkipReason]int) []catalogue.SkipReason {
	order := []catalogue.SkipReason{
		catalogue.ReasonUnauthorized,
		catalogue.ReasonWindow,
		catalogue.ReasonShape,
		catalogue.ReasonBlocked,
		catalogue.ReasonBudget,
		catalogue.ReasonNeverRan,
		catalogue.ReasonNotForceable,
		catalogue.ReasonUnclassified,
		catalogue.ReasonNone,
	}
	var present []catalogue.SkipReason
	for _, reason := range order {
		if counts[reason] > 0 {
			present = append(present, reason)
		}
	}
	return present
}

// wrapIndent breaks text at width and indents every line after the first,
// so a long sentence stays readable in a fixed-width terminal.
func wrapIndent(text string, width int, indent string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}

	var out strings.Builder
	line := words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			out.WriteString(line)
			out.WriteString("\n")
			out.WriteString(indent)
			line = word
			continue
		}
		line += " " + word
	}
	out.WriteString(line)
	return out.String()
}

// coherenceFindings restates the catalogue's reachability disagreements
// so the readable report can name them. The phrases are kept here because
// the catalogue package does not export the finding sentences.
func coherenceFindings(selected []catalogue.Check, records []catalogue.Record) []string {
	byID := make(map[catalogue.CheckID]catalogue.Record, len(records))
	for _, rec := range records {
		byID[rec.ID] = rec
	}
	var findings []string
	for _, entry := range selected {
		rec, found := byID[entry.ID]
		if !found {
			continue
		}
		findings = append(findings, checkFindings(entry, rec)...)
	}
	return findings
}

func checkFindings(entry catalogue.Check, rec catalogue.Record) []string {
	switch {
	case rec.State == catalogue.StateNotReached && rec.Reason == catalogue.ReasonUnclassified:
		return []string{
			fmt.Sprintf(
				"%s was skipped without classifying why, so nothing says whether another run could reach it",
				entry.ID,
			),
		}
	case rec.State == catalogue.StateNotReached && rec.Reason == catalogue.ReasonNeverRan:
		return []string{
			fmt.Sprintf("%s was selected but no check executed it", entry.ID),
		}
	case entry.Tier == catalogue.TierNotForceable && rec.State == catalogue.StateDemonstrated:
		return []string{
			fmt.Sprintf(
				"%s is catalogued as impossible to force but this run demonstrated it, so the catalogue is"+
					" stale",
				entry.ID,
			),
		}
	case rec.State == catalogue.StateNotReached && !tierExcuses(entry.Tier, rec.Reason):
		return []string{tierFinding(entry, rec.Reason)}
	}
	return nil
}

func tierExcuses(tier catalogue.CheckTier, reason catalogue.SkipReason) bool {
	switch reason {
	case catalogue.ReasonBudget, catalogue.ReasonBlocked, catalogue.ReasonNeverRan:
		return true
	case catalogue.ReasonUnauthorized:
		return tier == catalogue.TierAuthorized
	case catalogue.ReasonWindow, catalogue.ReasonShape:
		return tier == catalogue.TierOpportunistic
	case catalogue.ReasonNotForceable:
		return tier == catalogue.TierNotForceable
	default:
		return false
	}
}

func tierFinding(entry catalogue.Check, reason catalogue.SkipReason) string {
	switch entry.Tier {
	case catalogue.TierForceable:
		return fmt.Sprintf(
			"%s should be reachable by any run but was skipped as %q",
			entry.ID,
			reason,
		)
	case catalogue.TierAuthorized:
		return fmt.Sprintf(
			"%s needs its cost authorized, so a skip as %q does not explain it",
			entry.ID,
			reason,
		)
	case catalogue.TierOpportunistic:
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
