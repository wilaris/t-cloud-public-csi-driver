//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/reclaim"
)

const (
	// runPrefixRoot opens every name a run creates, so a sweep can find
	// its own resources without relying on the ownership marker. The
	// alphabet stays lower case.
	runPrefixRoot = "csi-e2e-"
	runIDBytes    = 5
)

// ledger is the authoritative record for teardown. The ownership marker
// is not: a run creates unmarked volumes to prove the driver refuses to
// adopt or delete them. A marker-scoped sweep would leak those.
type ledger struct {
	path string

	mu      sync.Mutex
	file    *os.File
	entries []reclaim.Entry
}

func newRunID() (string, error) {
	raw := make([]byte, runIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("draw run identifier: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func newLedger(path string) (*ledger, error) {
	//nolint:gosec // the path is built inside the run's own temporary directory
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	return &ledger{path: path, file: file}, nil
}

// record commits one reservation to durable storage before its create
// call is issued.
func (l *ledger) record(entry reclaim.Entry) error {
	entry.At = time.Now().UTC()

	l.mu.Lock()
	defer l.mu.Unlock()

	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode ledger entry: %w", err)
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write ledger entry: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("flush ledger entry: %w", err)
	}

	l.entries = append(l.entries, entry)
	return nil
}

// recordVolumeID attaches a server-issued volume identifier to a
// reservation already recorded by name.
func (l *ledger) recordVolumeID(name, volumeID string) error {
	return l.record(reclaim.Entry{
		Kind:     reclaim.ResourceVolume,
		Name:     name,
		VolumeID: volumeID,
	})
}

func (l *ledger) recorded() []reclaim.Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]reclaim.Entry(nil), l.entries...)
}

func (l *ledger) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// reserveAttachment commits the attachment reservation to the ledger
// before the attach is issued, the same discipline every create call
// follows.
func (h *harness) reserveAttachment(t *testing.T, volumeName, volumeID string) {
	t.Helper()

	if err := h.ledger.record(reclaim.Entry{
		Kind:     reclaim.ResourceAttachment,
		Name:     volumeName,
		VolumeID: volumeID,
		ServerID: h.identity.ServerID,
	}); err != nil {
		t.Fatalf("reserve attachment: %v", err)
	}
}

// teardown reclaims every resource the ledger reserved. It runs after a
// failed run too. It collects every failure so one bad entry does not
// hide the others.
func (h *harness) teardown(ctx context.Context) []error {
	var failures []error

	if err := ctx.Err(); err != nil {
		return []error{fmt.Errorf("teardown budget expired before cleanup: %w", err)}
	}
	if err := unmountAllUnder(ctx, h.runDir); err != nil {
		failures = append(failures, fmt.Errorf("release run mounts: %w", err))
	}

	entries := reclaim.ReclaimableEntries(h.ledger.recorded(), h.preexisting)
	resolved := map[string][]string{}
	for _, entry := range entries {
		if entry.VolumeID != "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			failures = append(
				failures,
				fmt.Errorf("teardown budget expired during resolution: %w", err),
			)
			return failures
		}
		if _, found := resolved[entry.Name]; !found {
			matches, err := h.findVolumeIDsByName(ctx, entry.Name)
			if err != nil {
				failures = append(
					failures,
					fmt.Errorf("resolve reserved volume %q: %w", entry.Name, err),
				)
				continue
			}
			resolved[entry.Name] = matches
		}
	}

	ids := reclaim.MergeReclamationIDs(entries, resolved, h.preexisting)
	failures = append(
		failures,
		reclaim.RunCleanupEffects(ctx, ids, func(ctx context.Context, volumeID string) error {
			if err := h.detachAndDelete(ctx, volumeID); err != nil {
				return fmt.Errorf("reclaim volume %s: %w", volumeID, err)
			}
			return nil
		})...)

	return failures
}

// sweep fails the run when a resource it created is still present. A
// leftover volume or attachment is an acceptance failure in its own
// right, so best-effort cleanup is not enough.
func (h *harness) sweep(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("teardown budget expired before survivor sweep: %w", err)
	}
	survivors, err := h.listVolumesWithPrefix(ctx, h.prefix)
	if err != nil {
		return fmt.Errorf("sweep for surviving resources: %w", err)
	}
	if len(survivors) == 0 {
		return nil
	}

	names := make([]string, 0, len(survivors))
	for _, volume := range survivors {
		names = append(names, volume.ID)
	}
	return fmt.Errorf(
		"run created %d volume(s) that survived teardown: %s",
		len(survivors),
		strings.Join(names, ", "),
	)
}
