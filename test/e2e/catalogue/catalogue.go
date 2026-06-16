// Package catalogue names the live proof asset's check obligations and
// evaluates what a run may claim about them.
//
// The catalogue is the list of what the asset checks. It can be printed
// without reaching the cloud. A check ends demonstrated, failed or not
// reached; a skip carries the reason that decides whether another run
// could reach it. Summarize turns those records into one verdict.
package catalogue

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

// CheckID names one catalogued check. It is also the subtest name, so it
// stays a stable slug that a test-name filter can select.
type CheckID string

// CheckTier says how much of a check a correctly configured run can force.
// It decides whether a check that was not demonstrated is a finding or an
// expected outcome.
type CheckTier string

const (
	// TierForceable is demonstrable by every run on a healthy instance.
	TierForceable CheckTier = "forceable"
	// TierAuthorized is demonstrable once the operator authorizes the resources it costs.
	TierAuthorized CheckTier = "authorized"
	// TierOpportunistic is demonstrable only when a transient window or the instance shape allows.
	TierOpportunistic CheckTier = "opportunistic"
	// TierNotForceable names a state neither the service nor a caller can produce on demand, so no run
	// of this asset demonstrates it. Its proof lives on a narrower seam.
	TierNotForceable CheckTier = "not forceable"
)

// Check is one catalogued proof obligation. The catalogue is the list of
// what the asset checks. It can be printed without reaching the cloud.
type Check struct {
	// ID is the stable slug a scenario and a test-name filter both use.
	ID CheckID
	// Group is the scenario that reports this check together with its neighbours.
	Group string
	// Tier is how much of this check a correctly configured run can force.
	Tier CheckTier
	// Profiles are the audiences that select this check.
	Profiles []settings.Profile
	// Statement is the behavior the check demonstrates when it holds.
	Statement string
	// ProofRoute names the narrower seam that proves a check no run of this
	// asset can force. It is empty on every other tier.
	ProofRoute string
}

var (
	evaluationAndProof = []settings.Profile{settings.ProfileEvaluation, settings.ProfileProof}
	proofOnly          = []settings.Profile{settings.ProfileProof}
)

// Check identifiers. Every scenario names one of these. The catalogue
// below lists every one, so a scenario naming an identifier that does not
// exist fails to compile.
const (
	CheckAttachmentNamesThisServer  CheckID = "attachment-names-this-server"
	CheckAttachedStateExplained     CheckID = "attached-volume-state-explained"
	CheckGuestExposesNamedDevice    CheckID = "guest-exposes-named-device"
	CheckStableLinkAgrees           CheckID = "stable-link-agrees-with-attachment"
	CheckBothBusNamings             CheckID = "both-bus-namings-observed"
	CheckRepeatedAttachIdempotent   CheckID = "repeated-attach-is-idempotent"
	CheckClassifiedFailureNoSecret  CheckID = "classified-failure-carries-no-credential"
	CheckCancellationReachesWire    CheckID = "cancellation-reaches-work-on-the-wire"
	CheckExt4LifecycleCompletes     CheckID = "ext4-lifecycle-completes"
	CheckXFSLifecycleCompletes      CheckID = "xfs-lifecycle-completes"
	CheckBlockLifecycleCompletes    CheckID = "block-lifecycle-completes"
	CheckNodeIdentityFromMetadata   CheckID = "node-identity-from-metadata"
	CheckControllerServesIdentity   CheckID = "controller-serves-identity"
	CheckControllerServesController CheckID = "controller-serves-controller-surface"
	CheckControllerServesNoNode     CheckID = "controller-serves-no-node-surface"
	CheckMarkerOnDetailSurface      CheckID = "marker-on-detail-surface"
	CheckMarkerOnListSurface        CheckID = "marker-on-list-surface"
	CheckOffsetPagingAdvances       CheckID = "offset-paging-advances"
	CheckUnmarkedNotAdopted         CheckID = "unmarked-same-name-not-adopted"
	CheckIncompatibleRefused        CheckID = "incompatible-same-name-refused"
	CheckDuplicateIsDeterministic   CheckID = "duplicate-name-is-deterministic"
	CheckDeletingNotAdopted         CheckID = "deleting-volume-not-adopted"
	CheckLaterPageRevealsCandidate  CheckID = "later-page-reveals-candidate"
	CheckNoCallerValueInTags        CheckID = "no-caller-value-in-tags-or-metadata"
	CheckUnmarkedDeletionRefused    CheckID = "unmarked-deletion-refused"
	CheckAbsentDeletionReported     CheckID = "absent-deletion-reports-absence"
	CheckInFlightDeletionObserved   CheckID = "in-flight-deletion-observed"
	CheckFailedDeletionReported     CheckID = "failed-deletion-reported-as-failure"
	CheckAbsenceNeverArrives        CheckID = "absence-never-arrives-reported-as-failure"
)

// Group names. A group is one scenario. A run reports its checks together.
const (
	GroupAttachment   = "attachment handoff"
	GroupCancellation = "attachment cancellation"
	GroupLifecycle    = "volume lifecycle"
	GroupStartup      = "role startup"
	GroupContract     = "volume contract"
	GroupDeletion     = "volume deletion"
)

// checks lists every obligation the asset carries, in the order a full run
// reaches them. Groups stay contiguous so a progress counter and a report
// table can share one walk.
var checks = []Check{
	{
		ID:        CheckAttachmentNamesThisServer,
		Group:     GroupAttachment,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "an attachment reports the volume, the server the run executes on and a device name",
	},
	{
		ID:        CheckAttachedStateExplained,
		Group:     GroupAttachment,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "the cloud reports a volume state the attachment explains",
	},
	{
		ID:        CheckGuestExposesNamedDevice,
		Group:     GroupAttachment,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "the guest exposes a block device under the name the cloud reported",
	},
	{
		ID:        CheckStableLinkAgrees,
		Group:     GroupAttachment,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "a stable link names the attached volume and resolves to the same kernel device the cloud named",
	},
	{
		ID:        CheckBothBusNamings,
		Group:     GroupAttachment,
		Tier:      TierOpportunistic,
		Profiles:  evaluationAndProof,
		Statement: "both bus namings are observed, which one instance can show only when it exposes both disk buses",
	},
	{
		ID:        CheckRepeatedAttachIdempotent,
		Group:     GroupAttachment,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "attaching an already-attached volume returns the same descriptor",
	},
	{
		ID:        CheckClassifiedFailureNoSecret,
		Group:     GroupCancellation,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "a classified cloud failure carries a matchable sentinel and no credential",
	},
	{
		ID:        CheckCancellationReachesWire,
		Group:     GroupCancellation,
		Tier:      TierOpportunistic,
		Profiles:  proofOnly,
		Statement: "canceling reaches work already on the wire. Re-issuing the request reconciles without creating a second attach",
	},
	{
		ID:        CheckExt4LifecycleCompletes,
		Group:     GroupLifecycle,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "a mounted ext4 volume completes create, attach, stage, publish, read back and teardown. Every idempotent call repeats safely",
	},
	{
		ID:        CheckXFSLifecycleCompletes,
		Group:     GroupLifecycle,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "a mounted xfs volume completes the same lifecycle",
	},
	{
		ID:        CheckBlockLifecycleCompletes,
		Group:     GroupLifecycle,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "a raw block volume completes the same lifecycle and publishes a block device and not a filesystem",
	},
	{
		ID:        CheckNodeIdentityFromMetadata,
		Group:     GroupStartup,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "the node role resolves its node identity and zone from the metadata service",
	},
	{
		ID:        CheckControllerServesIdentity,
		Group:     GroupStartup,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "the controller role serves its identity once authentication succeeded",
	},
	{
		ID:        CheckControllerServesController,
		Group:     GroupStartup,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "the controller role serves the controller surface",
	},
	{
		ID:        CheckControllerServesNoNode,
		Group:     GroupStartup,
		Tier:      TierForceable,
		Profiles:  evaluationAndProof,
		Statement: "the controller role serves no node surface",
	},
	{
		ID:        CheckMarkerOnDetailSurface,
		Group:     GroupContract,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "the ownership marker is accepted and returned on the volume detail surface",
	},
	{
		ID:        CheckMarkerOnListSurface,
		Group:     GroupContract,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "the ownership marker is returned on the discovery list surface",
	},
	{
		ID:        CheckOffsetPagingAdvances,
		Group:     GroupContract,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "offset paging advances between pages and does not repeat a page",
	},
	{
		ID:        CheckUnmarkedNotAdopted,
		Group:     GroupContract,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "an unmarked volume of the same name is neither adopted nor modified",
	},
	{
		ID:        CheckIncompatibleRefused,
		Group:     GroupContract,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "a marked but incompatible volume of the same name is refused and left intact",
	},
	{
		ID:        CheckDuplicateIsDeterministic,
		Group:     GroupContract,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "two candidates of the same name produce one deterministic answer",
	},
	{
		ID:        CheckDeletingNotAdopted,
		Group:     GroupContract,
		Tier:      TierOpportunistic,
		Profiles:  proofOnly,
		Statement: "a volume being deleted is not adopted, which a run can show only while that transient state lasts",
	},
	{
		ID:        CheckLaterPageRevealsCandidate,
		Group:     GroupContract,
		Tier:      TierAuthorized,
		Profiles:  proofOnly,
		Statement: "a candidate of the same name is still found on a later list page, which needs more volumes than one discovery page holds",
	},
	{
		ID:        CheckNoCallerValueInTags,
		Group:     GroupContract,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "no caller-supplied value reaches volume tags or metadata",
	},
	{
		ID:        CheckUnmarkedDeletionRefused,
		Group:     GroupDeletion,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "deleting an unmarked volume is refused and leaves it intact",
	},
	{
		ID:        CheckAbsentDeletionReported,
		Group:     GroupDeletion,
		Tier:      TierForceable,
		Profiles:  proofOnly,
		Statement: "deleting a volume that is already gone reports its absence",
	},
	{
		ID:        CheckInFlightDeletionObserved,
		Group:     GroupDeletion,
		Tier:      TierOpportunistic,
		Profiles:  proofOnly,
		Statement: "a deletion already in progress is observed and not re-issued, which a run can show only while that transient state lasts",
	},
	{
		ID:         CheckFailedDeletionReported,
		Group:      GroupDeletion,
		Tier:       TierNotForceable,
		Profiles:   proofOnly,
		Statement:  "a terminally failed deletion is reported as a failure; neither the service nor a caller can produce that state on demand",
		ProofRoute: "an offline HTTP transport script reports error_deleting, requires terminal failure and verifies that no destructive request is issued",
	},
	{
		ID:         CheckAbsenceNeverArrives,
		Group:      GroupDeletion,
		Tier:       TierNotForceable,
		Profiles:   proofOnly,
		Statement:  "a deletion whose absence never arrives is reported as a failure; the service cannot be made to accept a deletion and then keep the volume",
		ProofRoute: "an offline HTTP transport script accepts deletion, keeps reporting the volume present through the bounded observation attempts and requires terminal failure",
	},
}

// checkIndex resolves an identifier to its catalogue entry.
var checkIndex = indexChecks(checks)

func indexChecks(cat []Check) map[CheckID]Check {
	byID := make(map[CheckID]Check, len(cat))
	for _, entry := range cat {
		byID[entry.ID] = entry
	}
	return byID
}

func copyCheck(entry Check) Check {
	if entry.Profiles != nil {
		entry.Profiles = append([]settings.Profile(nil), entry.Profiles...)
	}
	return entry
}

// LookupCheck reports the catalogue entry for id.
func LookupCheck(id CheckID) (Check, bool) {
	entry, found := checkIndex[id]
	if !found {
		return Check{}, false
	}
	return copyCheck(entry), true
}

// SelectCatalogue returns the entries explicitly assigned to p, preserving
// catalogue order.
func SelectCatalogue(p settings.Profile) []Check {
	selected := make([]Check, 0, len(checks))
	for _, entry := range checks {
		if !slices.Contains(entry.Profiles, p) {
			continue
		}
		selected = append(selected, copyCheck(entry))
	}
	return selected
}

func profileNames(profiles []settings.Profile) string {
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}

// groupsOf returns the group names in catalogue order, without repeats.
func groupsOf(cat []Check) []string {
	var groups []string
	for _, entry := range cat {
		if len(groups) == 0 || groups[len(groups)-1] != entry.Group {
			groups = append(groups, entry.Group)
		}
	}
	return groups
}

// WriteCatalogue prints what the asset checks, so it can be read before a
// run is paid for.
func WriteCatalogue(w io.Writer, cat []Check) error {
	tab := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, err := fmt.Fprintf(tab, "%d checks in %d scenarios.\n\n", len(cat), len(groupsOf(cat)))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(
		tab,
		"#\tSCENARIO\tCHECK\tPROFILES\tREACHABLE\tWHAT IT DEMONSTRATES\tOFFLINE PROOF ROUTE",
	)
	if err != nil {
		return err
	}
	for i, entry := range cat {
		_, err = fmt.Fprintf(
			tab,
			"%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			i+1,
			entry.Group,
			entry.ID,
			profileNames(entry.Profiles),
			entry.Tier,
			entry.Statement,
			entry.ProofRoute,
		)
		if err != nil {
			return err
		}
	}
	if err = tab.Flush(); err != nil {
		return err
	}

	counts := map[CheckTier]int{}
	for _, entry := range cat {
		counts[entry.Tier]++
	}
	_, err = fmt.Fprintf(
		w,
		"\n%d reachable by any run, %d once their cost is authorized, %d only when a transient state or"+
			" the instance shape allows and %d that no run of this asset can force.\n",
		counts[TierForceable],
		counts[TierAuthorized],
		counts[TierOpportunistic],
		counts[TierNotForceable],
	)
	return err
}
