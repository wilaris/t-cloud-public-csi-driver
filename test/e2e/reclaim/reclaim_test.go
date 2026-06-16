package reclaim

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestReclaimableEntries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		entries     []Entry
		preexisting map[string]bool
		want        []Entry
	}{
		{
			name: "keeps every volume sharing one name",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-duplicate"},
				{Kind: ResourceVolume, Name: "csi-e2e-1-duplicate", VolumeID: "vol-first"},
				{Kind: ResourceVolume, Name: "csi-e2e-1-duplicate"},
				{Kind: ResourceVolume, Name: "csi-e2e-1-duplicate", VolumeID: "vol-second"},
			},
			want: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-duplicate", VolumeID: "vol-first"},
				{Kind: ResourceVolume, Name: "csi-e2e-1-duplicate", VolumeID: "vol-second"},
			},
		},
		{
			name: "drops a preexisting volume",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-marker"},
				{Kind: ResourceVolume, Name: "csi-e2e-1-marker", VolumeID: "vol-preexisting"},
				{Kind: ResourceVolume, Name: "csi-e2e-1-marker", VolumeID: "vol-created"},
			},
			preexisting: map[string]bool{"vol-preexisting": true},
			want: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-marker", VolumeID: "vol-created"},
			},
		},
		{
			name: "keeps an unidentified reservation once",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-interrupted"},
				{Kind: ResourceVolume, Name: "csi-e2e-1-interrupted"},
				{Kind: ResourceAttachment, ServerID: "srv-1", VolumeID: "vol-attached"},
			},
			want: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-interrupted"},
			},
		},
		{
			name: "ignores a resolved reservation line",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-marker"},
				{Kind: ResourceVolume, Name: "csi-e2e-1-marker", VolumeID: "vol-created"},
			},
			want: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-marker", VolumeID: "vol-created"},
			},
		},
		{
			name: "preserves mixed same-name reservations, resolved then unresolved",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "same"},
				{Kind: ResourceVolume, Name: "same", VolumeID: "known"},
				{Kind: ResourceVolume, Name: "same"},
			},
			want: []Entry{
				{Kind: ResourceVolume, Name: "same", VolumeID: "known"},
				{Kind: ResourceVolume, Name: "same"},
			},
		},
		{
			name: "preserves mixed same-name reservations, unresolved then later success",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "same"},
				{Kind: ResourceVolume, Name: "same"},
				{Kind: ResourceVolume, Name: "same", VolumeID: "known"},
			},
			want: []Entry{
				{Kind: ResourceVolume, Name: "same", VolumeID: "known"},
				{Kind: ResourceVolume, Name: "same"},
			},
		},
		{
			name:    "empty ledger yields nothing",
			entries: nil,
			want:    nil,
		},
		{
			name: "nil preexisting is an empty set",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-only", VolumeID: "vol-only"},
			},
			preexisting: nil,
			want: []Entry{
				{Kind: ResourceVolume, Name: "csi-e2e-1-only", VolumeID: "vol-only"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ReclaimableEntries(tc.entries, tc.preexisting)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ReclaimableEntries() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestMergeReclamationIDs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		entries     []Entry
		resolved    map[string][]string
		preexisting map[string]bool
		want        []string
	}{
		{
			name: "excludes preexisting and deduplicates resolved identifiers",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "same", VolumeID: "known"},
				{Kind: ResourceVolume, Name: "same"},
			},
			resolved: map[string][]string{
				"same": {"known", "ambiguous", "ambiguous", "preexisting"},
			},
			preexisting: map[string]bool{"preexisting": true},
			want:        []string{"known", "ambiguous"},
		},
		{
			name: "name-only row with no resolved matches contributes nothing",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "missing"},
			},
			resolved: map[string][]string{},
			want:     nil,
		},
		{
			name: "drops an identified preexisting volume",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "kept", VolumeID: "vol-created"},
				{Kind: ResourceVolume, Name: "old", VolumeID: "vol-preexisting"},
			},
			preexisting: map[string]bool{"vol-preexisting": true},
			want:        []string{"vol-created"},
		},
		{
			name: "empty resolved map leaves identified identifiers",
			entries: []Entry{
				{Kind: ResourceVolume, Name: "named", VolumeID: "vol-known"},
				{Kind: ResourceVolume, Name: "named"},
			},
			resolved: map[string][]string{},
			want:     []string{"vol-known"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := MergeReclamationIDs(tc.entries, tc.resolved, tc.preexisting)
			if !slices.Equal(got, tc.want) {
				t.Errorf("MergeReclamationIDs() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunCleanupEffects(t *testing.T) {
	t.Parallel()

	t.Run("stops starting later effects after cancellation", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		var started []string
		failures := RunCleanupEffects(
			ctx,
			[]string{"first", "second"},
			func(_ context.Context, id string) error {
				started = append(started, id)
				cancel()
				return nil
			},
		)
		if !slices.Equal(started, []string{"first"}) {
			t.Errorf("started effects = %v, want %v", started, []string{"first"})
		}
		if len(failures) != 1 || !errors.Is(failures[0], context.Canceled) {
			t.Errorf("RunCleanupEffects() failures = %v, want cancellation", failures)
		}
	})

	t.Run("collects every effect error and continues", func(t *testing.T) {
		t.Parallel()

		first := errors.New("detach first")
		third := errors.New("delete third")
		var started []string
		failures := RunCleanupEffects(
			t.Context(),
			[]string{"first", "second", "third"},
			func(_ context.Context, id string) error {
				started = append(started, id)
				switch id {
				case "first":
					return first
				case "third":
					return third
				default:
					return nil
				}
			},
		)
		if !slices.Equal(started, []string{"first", "second", "third"}) {
			t.Errorf("started effects = %v, want %v", started, []string{"first", "second", "third"})
		}
		if len(failures) != 2 || !errors.Is(failures[0], first) || !errors.Is(failures[1], third) {
			t.Errorf("RunCleanupEffects() failures = %v, want [%v %v]", failures, first, third)
		}
	})

	t.Run("already cancelled starts no effect", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		var started []string
		failures := RunCleanupEffects(
			ctx,
			[]string{"first", "second"},
			func(_ context.Context, id string) error {
				started = append(started, id)
				return nil
			},
		)
		if len(started) != 0 {
			t.Errorf("started effects = %v, want none", started)
		}
		if len(failures) != 1 || !errors.Is(failures[0], context.Canceled) {
			t.Errorf("RunCleanupEffects() failures = %v, want cancellation", failures)
		}
	})

	t.Run("empty identifier list starts nothing", func(t *testing.T) {
		t.Parallel()

		var started []string
		failures := RunCleanupEffects(t.Context(), nil, func(_ context.Context, id string) error {
			started = append(started, id)
			return nil
		})
		if len(started) != 0 {
			t.Errorf("started effects = %v, want none", started)
		}
		if failures != nil {
			t.Errorf("RunCleanupEffects() failures = %v, want nil", failures)
		}
	})
}
