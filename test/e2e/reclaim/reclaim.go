// Package reclaim reduces a run's resource ledger to the distinct volumes teardown must delete,
// and applies those deletions as cleanup effects.

// A ledger line is reserved before the create is issued, so an interrupted create is still
// reclaimable. Volumes the preflight snapshot already showed are never scheduled. This package does
// not call the cloud.
package reclaim

import (
	"context"
	"time"
)

// ResourceKind names what a ledger line reserves.
type ResourceKind string

const (
	// ResourceVolume is a volume the run intended to create.
	ResourceVolume ResourceKind = "volume"
	// ResourceAttachment is an attachment the run intended to create.
	ResourceAttachment ResourceKind = "attachment"
)

// Entry is one durable reservation. The harness records it before
// issuing create, so a crash still leaves a name or identifier teardown
// can resolve. Only the chosen name and any server-issued identifiers
// belong here.
type Entry struct {
	// Kind is the kind of resource this line reserved.
	Kind ResourceKind `json:"kind"`
	// Name is the harness-chosen volume name. It is empty on some attachment rows.
	Name string `json:"name,omitempty"`
	// VolumeID is the server-issued identifier, empty until create reports one.
	VolumeID string `json:"volume_id,omitempty"`
	// ServerID is the instance the attachment was reserved against.
	ServerID string `json:"server_id,omitempty"`
	// At is when the reservation was committed to the ledger.
	At time.Time `json:"at"`
}

// ReclaimableEntries returns the volumes teardown still has to delete. Each observed identifier is
// kept once, because one name may cover several created volumes. A name that was reserved more
// times than it was identified is kept once as a name-only row so an interrupted create can still
// be found by name. Identifiers present in preexisting are dropped.
func ReclaimableEntries(entries []Entry, preexisting map[string]bool) []Entry {
	var out []Entry
	seenID := map[string]bool{}
	reservations := map[string]int{}
	observations := map[string]int{}

	for _, entry := range entries {
		if entry.Kind != ResourceVolume {
			continue
		}
		if entry.VolumeID == "" {
			reservations[entry.Name]++
			continue
		}
		observations[entry.Name]++
		if seenID[entry.VolumeID] || preexisting[entry.VolumeID] {
			continue
		}
		seenID[entry.VolumeID] = true
		out = append(out, entry)
	}

	seenName := map[string]bool{}
	for _, entry := range entries {
		if entry.Kind != ResourceVolume || entry.VolumeID != "" {
			continue
		}
		if reservations[entry.Name] <= observations[entry.Name] || seenName[entry.Name] {
			continue
		}
		seenName[entry.Name] = true
		out = append(out, entry)
	}

	return out
}

// MergeReclamationIDs builds the identifier list cleanup will visit.
// Known ledger identifiers come first; name-only rows then contribute
// exact-name matches from resolved. Pre-existing identifiers and
// duplicates are skipped so each remaining identifier is scheduled once.
func MergeReclamationIDs(
	entries []Entry,
	resolved map[string][]string,
	preexisting map[string]bool,
) []string {
	var ids []string
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.VolumeID != "" {
			if preexisting[entry.VolumeID] || seen[entry.VolumeID] {
				continue
			}
			seen[entry.VolumeID] = true
			ids = append(ids, entry.VolumeID)
			continue
		}
		for _, id := range resolved[entry.Name] {
			if preexisting[id] || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// RunCleanupEffects applies effect to each identifier in order. It stops
// starting later effects once ctx is done. It collects every effect
// error so one failed delete cannot hide the rest.
func RunCleanupEffects(
	ctx context.Context,
	ids []string,
	effect func(context.Context, string) error,
) []error {
	var failures []error
	for _, id := range ids {
		err := ctx.Err()
		if err != nil {
			failures = append(failures, err)
			break
		}
		err = effect(ctx, id)
		if err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}
