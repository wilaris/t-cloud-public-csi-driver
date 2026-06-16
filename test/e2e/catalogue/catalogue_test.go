package catalogue

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

func TestCatalogueEntriesAreWellFormed(t *testing.T) {
	t.Parallel()

	seen := map[CheckID]bool{}
	for _, entry := range checks {
		if seen[entry.ID] {
			t.Errorf("check %q appears twice", entry.ID)
		}
		seen[entry.ID] = true

		if entry.ID == "" {
			t.Error("a check carries no identifier")
		}
		if got := string(entry.ID); got != strings.ToLower(got) || strings.ContainsAny(got, " \t") {
			t.Errorf(
				"check %q must be a lower-case slug so it survives as a subtest name",
				entry.ID,
			)
		}
		if entry.Group == "" {
			t.Errorf("check %q names no scenario", entry.ID)
		}
		if entry.Statement == "" {
			t.Errorf("check %q states nothing it demonstrates", entry.ID)
		}
		if len(entry.Profiles) == 0 {
			t.Errorf("check %q belongs to no profile", entry.ID)
		}
		profileSeen := map[settings.Profile]bool{}
		for _, p := range entry.Profiles {
			if profileSeen[p] {
				t.Errorf("check %q repeats profile %q", entry.ID, p)
			}
			profileSeen[p] = true
		}
		if entry.Tier == TierNotForceable && entry.ProofRoute == "" {
			t.Errorf("check %q names no offline proof route", entry.ID)
		}
		if entry.Tier != TierNotForceable && entry.ProofRoute != "" {
			t.Errorf("check %q names a proof route despite being %q", entry.ID, entry.Tier)
		}
	}
}

func TestLookupCheckFindsEveryCataloguedID(t *testing.T) {
	t.Parallel()

	ids := []CheckID{
		CheckAttachmentNamesThisServer,
		CheckAttachedStateExplained,
		CheckGuestExposesNamedDevice,
		CheckStableLinkAgrees,
		CheckBothBusNamings,
		CheckRepeatedAttachIdempotent,
		CheckClassifiedFailureNoSecret,
		CheckCancellationReachesWire,
		CheckExt4LifecycleCompletes,
		CheckXFSLifecycleCompletes,
		CheckBlockLifecycleCompletes,
		CheckNodeIdentityFromMetadata,
		CheckControllerServesIdentity,
		CheckControllerServesController,
		CheckControllerServesNoNode,
		CheckMarkerOnDetailSurface,
		CheckMarkerOnListSurface,
		CheckOffsetPagingAdvances,
		CheckUnmarkedNotAdopted,
		CheckIncompatibleRefused,
		CheckDuplicateIsDeterministic,
		CheckDeletingNotAdopted,
		CheckLaterPageRevealsCandidate,
		CheckNoCallerValueInTags,
		CheckUnmarkedDeletionRefused,
		CheckAbsentDeletionReported,
		CheckInFlightDeletionObserved,
		CheckFailedDeletionReported,
		CheckAbsenceNeverArrives,
	}
	if len(ids) != 29 {
		t.Fatalf("LookupCheck table has %d identifiers, want 29", len(ids))
	}
	for _, id := range ids {
		got, found := LookupCheck(id)
		if !found {
			t.Errorf("LookupCheck(%q) found = false, want true", id)
			continue
		}
		if got.ID != id {
			t.Errorf("LookupCheck(%q).ID = %q, want %q", id, got.ID, id)
		}
	}

	got, found := LookupCheck("not-a-catalogued-check")
	if found {
		t.Errorf("LookupCheck(unknown) found = true, want false")
	}
	if got.ID != "" || got.Group != "" || got.Tier != "" || got.Statement != "" ||
		got.ProofRoute != "" || got.Profiles != nil {
		t.Errorf("LookupCheck(unknown) = %+v, want the zero Check", got)
	}
}

func TestProfileSelections(t *testing.T) {
	t.Parallel()

	evaluation := SelectCatalogue(settings.ProfileEvaluation)
	proof := SelectCatalogue(settings.ProfileProof)
	if len(evaluation) == 0 || len(proof) == 0 {
		t.Fatalf(
			"profile selections must be non-empty: evaluation=%d proof=%d",
			len(evaluation),
			len(proof),
		)
	}
	if len(evaluation) != 13 {
		t.Errorf("SelectCatalogue(evaluation) = %d checks, want 13", len(evaluation))
	}
	if len(proof) != len(checks) {
		t.Errorf("SelectCatalogue(proof) = %d checks, want all %d", len(proof), len(checks))
	}

	wantGroups := map[string]bool{
		GroupAttachment: true,
		GroupLifecycle:  true,
		GroupStartup:    true,
	}
	for _, entry := range evaluation {
		if !wantGroups[entry.Group] {
			t.Errorf("evaluation unexpectedly selected %q from %q", entry.ID, entry.Group)
		}
	}
	for _, entry := range checks {
		if !wantGroups[entry.Group] {
			continue
		}
		if !slices.ContainsFunc(evaluation, func(selected Check) bool {
			return selected.ID == entry.ID
		}) {
			t.Errorf("evaluation omitted %q from %q", entry.ID, entry.Group)
		}
	}

	unknown := SelectCatalogue(settings.Profile("not-a-profile"))
	if len(unknown) != 0 {
		t.Errorf("SelectCatalogue(unknown) = %d checks, want 0", len(unknown))
	}
}

func TestCatalogueGroupsAreContiguous(t *testing.T) {
	t.Parallel()

	started := map[string]bool{}
	previous := ""
	for _, entry := range checks {
		if entry.Group == previous {
			continue
		}
		if started[entry.Group] {
			t.Errorf("scenario %q resumes after another scenario interrupted it", entry.Group)
		}
		started[entry.Group] = true
		previous = entry.Group
	}
}

func TestWriteCatalogueNamesEveryCheck(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := WriteCatalogue(&out, checks); err != nil {
		t.Fatalf("WriteCatalogue: %v", err)
	}

	printed := out.String()
	for _, entry := range checks {
		if !strings.Contains(printed, string(entry.ID)) {
			t.Errorf("the printed catalogue omits %q", entry.ID)
		}
	}
	for _, group := range groupsOf(checks) {
		if !strings.Contains(printed, group) {
			t.Errorf("the printed catalogue omits scenario %q", group)
		}
	}
	for _, expected := range []string{
		"PROFILES",
		"OFFLINE PROOF ROUTE",
		string(settings.ProfileEvaluation),
		string(settings.ProfileProof),
	} {
		if !strings.Contains(printed, expected) {
			t.Errorf("the printed catalogue omits %q", expected)
		}
	}
	for _, entry := range checks {
		if entry.ProofRoute != "" && !strings.Contains(printed, entry.ProofRoute) {
			t.Errorf("the printed catalogue omits the proof route for %q", entry.ID)
		}
	}

	counts := map[CheckTier]int{}
	for _, entry := range checks {
		counts[entry.Tier]++
	}
	if counts[TierForceable] != 22 || counts[TierAuthorized] != 1 ||
		counts[TierOpportunistic] != 4 || counts[TierNotForceable] != 2 {
		t.Fatalf(
			"live mix = forceable %d authorized %d opportunistic %d not-forceable %d, want 22/1/4/2",
			counts[TierForceable],
			counts[TierAuthorized],
			counts[TierOpportunistic],
			counts[TierNotForceable],
		)
	}
	footer := "22 reachable by any run, 1 once their cost is authorized, 4 only when a transient"
	if !strings.Contains(printed, footer) {
		t.Errorf("the printed catalogue omits the live tier counts, got:\n%s", printed)
	}
}

func TestReturnedChecksDoNotAliasPackageState(t *testing.T) {
	t.Parallel()

	selected := SelectCatalogue(settings.ProfileEvaluation)
	if len(selected) == 0 || len(selected[0].Profiles) == 0 {
		t.Fatal("SelectCatalogue(evaluation) returned no profiles to mutate")
	}
	selected[0].Profiles[0] = "mutated"

	got, found := LookupCheck(selected[0].ID)
	if !found {
		t.Fatalf("LookupCheck(%q) found = false, want true", selected[0].ID)
	}
	if got.Profiles[0] == "mutated" {
		t.Errorf("SelectCatalogue profiles alias package state")
	}

	looked, found := LookupCheck(CheckAttachmentNamesThisServer)
	if !found || len(looked.Profiles) == 0 {
		t.Fatal("LookupCheck(attachment-names-this-server) returned no profiles to mutate")
	}
	looked.Profiles[0] = "mutated"
	again, _ := LookupCheck(CheckAttachmentNamesThisServer)
	if again.Profiles[0] == "mutated" {
		t.Errorf("LookupCheck profiles alias package state")
	}
}
