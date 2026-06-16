package report

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/catalogue"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

// FinalOutputs is the fully rendered report and evidence, ready to commit.
type FinalOutputs struct {
	// Report is the masked readable report, including the header.
	Report []byte
	// Evidence is the masked JSON evidence record.
	Evidence []byte
}

// OutputDestinations names where a run writes its two artifacts.
type OutputDestinations struct {
	// ReportPath is the readable report destination. Empty means standard output.
	ReportPath string
	// EvidencePath is the JSON evidence destination. It is never empty after resolve.
	EvidencePath string
}

// ResolveOutputDestinations fills the evidence path when the operator did
// not name one. It refuses a pair that would open as one file.
func ResolveOutputDestinations(set *settings.Settings, runID string) (OutputDestinations, error) {
	destinations := OutputDestinations{
		ReportPath:   set.ReportPath,
		EvidencePath: set.EvidencePath,
	}
	if destinations.EvidencePath == "" {
		destinations.EvidencePath = "./conformance-" + runID + ".json"
	}
	if destinations.ReportPath == "" {
		return destinations, nil
	}
	same, err := sameOutputPath(destinations.ReportPath, destinations.EvidencePath)
	if err != nil {
		return OutputDestinations{}, fmt.Errorf(
			"report and evidence destinations could not be told apart: %w",
			err,
		)
	}
	if same {
		return OutputDestinations{}, errors.New("report and evidence destinations must be distinct")
	}
	return destinations, nil
}

// sameOutputPath reports whether the two destinations name one file, resolving symlinks so an alias
// cannot slip past a lexical comparison. Both files are opened with truncation, so a pair this
// cannot tell apart is rejected.
func sameOutputPath(left, right string) (bool, error) {
	leftResolved, err := resolveDestination(left)
	if err != nil {
		return false, err
	}
	rightResolved, err := resolveDestination(right)
	if err != nil {
		return false, err
	}
	return leftResolved == rightResolved, nil
}

// resolveDestination canonicalizes one destination. The file itself may not exist yet, so a missing
// final element is resolved through its parent directory instead. A path that exists but does not
// resolve (a dangling symlink) is refused, because opening it would create its hidden target.
func resolveDestination(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, statErr := os.Lstat(abs); statErr == nil {
		return "", fmt.Errorf("%s exists but does not resolve to a file", abs)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// StageFinalOutputs renders the readable report and the JSON evidence
// through the run's mask. No destination is opened: a render or
// containment failure leaves the operator's files untouched.
func StageFinalOutputs(
	mask func(string) string,
	head ReportHeader,
	records []catalogue.Record,
	selected []catalogue.Check,
	result catalogue.Summary,
	teardown []error,
	finalization []error,
	observations []NameValue,
	elapsed time.Duration,
	ev *Evidence,
	contain func(string, ...string) error,
) (FinalOutputs, error) {
	if mask == nil {
		mask = func(line string) string { return line }
	}
	if ev == nil {
		return FinalOutputs{}, errors.New("evidence record is required")
	}

	report, err := renderMasked(mask, func(w io.Writer) error {
		if headerErr := WriteHeader(w, head); headerErr != nil {
			return headerErr
		}
		return WriteReport(
			w,
			head,
			selected,
			records,
			result,
			teardown,
			finalization,
			observations,
			elapsed,
		)
	})
	if err != nil {
		return FinalOutputs{}, fmt.Errorf("render report: %w", err)
	}
	machine, err := renderMasked(mask, func(w io.Writer) error {
		return ev.Render(w, selected, result)
	})
	if err != nil {
		return FinalOutputs{}, fmt.Errorf("render evidence: %w", err)
	}
	if contain != nil {
		if err = contain(
			"complete report and evidence",
			string(report),
			string(machine),
		); err != nil {
			return FinalOutputs{}, err
		}
	}
	return FinalOutputs{Report: report, Evidence: machine}, nil
}

func renderMasked(mask func(string) string, render func(io.Writer) error) ([]byte, error) {
	var staged bytes.Buffer
	masked := NewMaskingWriter(&staged, mask)
	renderErr := render(masked)
	flushErr := masked.Flush()
	if err := errors.Join(renderErr, flushErr); err != nil {
		return nil, err
	}
	return append([]byte(nil), staged.Bytes()...), nil
}

// CommitFinalOutputs writes the staged artifacts. An empty report path
// goes to stdout; the evidence path is always a file.
func CommitFinalOutputs(
	dest OutputDestinations,
	outputs FinalOutputs,
	stdout io.Writer,
	open func(string) (io.WriteCloser, error),
) error {
	if open == nil {
		open = OpenOutput
	}
	var failures []error
	if dest.ReportPath == "" {
		if err := writeComplete(stdout, outputs.Report); err != nil {
			failures = append(failures, fmt.Errorf("write report: %w", err))
		}
	} else if err := writeOutputFile(dest.ReportPath, outputs.Report, open); err != nil {
		failures = append(failures, fmt.Errorf("write report: %w", err))
	}
	if err := writeOutputFile(dest.EvidencePath, outputs.Evidence, open); err != nil {
		failures = append(failures, fmt.Errorf("write evidence: %w", err))
	}
	return errors.Join(failures...)
}

func writeOutputFile(path string, content []byte, open func(string) (io.WriteCloser, error)) error {
	file, err := open(path)
	if err != nil {
		return err
	}
	writeErr := writeComplete(file, content)
	var syncErr error
	if writeErr == nil {
		if synced, ok := file.(interface{ Sync() error }); ok {
			syncErr = synced.Sync()
		}
	}
	return errors.Join(writeErr, syncErr, file.Close())
}

func writeComplete(w io.Writer, content []byte) error {
	written, err := w.Write(content)
	if err != nil {
		return err
	}
	if written != len(content) {
		return io.ErrShortWrite
	}
	return nil
}

// OpenOutput creates or truncates path with permissions that keep the
// run's record away from other accounts on the host.
func OpenOutput(path string) (io.WriteCloser, error) {
	//nolint:gosec // the path is the operator's chosen destination for this run's output
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
}
