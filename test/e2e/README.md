# Checking this driver against your own project

This directory builds `t-cloud-csi-conformance`, a single self-contained binary that runs the driver
against one real T Cloud Public project and reports, check by check, whether it worked.

Read this page before you run it. A run mutates real cloud state and costs real money.

## What a run does to your project

- It creates, attaches, formats, mounts, reads, unmounts, detaches and deletes real volumes.
- It deliberately creates a few volumes the driver does not own, to confirm the driver refuses to adopt
  or delete them.
- Everything it creates carries a run-unique `csi-e2e-<run id>-` name prefix. It writes each
  intended resource to a local ledger *before* asking the cloud for it, so an interrupted run is still
  reclaimable by name.
- It reclaims everything it created, after failures as well as successes. It **fails the run** if any
  resource it created is still there at the end.
- It never touches a volume it did not create. Volumes that already carry the driver's ownership marker
  are recorded at startup and skipped by reclamation.

## What you need

- A T Cloud Public project you are willing to have mutated. Use a throwaway project, not one holding
  anything else. Nothing in the run can tell the difference, so the assertion is yours to make.
- One compute instance in that project, which you destroy afterwards. Credentials reach the instance for
  the run and are not meant to outlive it.
- Root on that instance. The node checks attach, format and mount real block devices.
- An access key pair scoped to that project. Also the name of a volume type your region offers. Volume
  types are regional and this asset carries no inventory of them, so you name one.

## Running it

Build on any machine with a Go toolchain:

```sh
make e2e-build
```

That writes `dist/conformance/t-cloud-csi-conformance` and a copy of the driver binary beside it, both
stamped with the same build identity. Copy both files onto your instance. The instance needs neither a
Go toolchain nor a source checkout.

See what would be checked, before you spend anything:

```sh
./t-cloud-csi-conformance -list-checks
```

Then run it as root:

```sh
export OS_AUTH_URL=... OS_PROJECT_ID=... OS_REGION_NAME=...
export OS_ACCESS_KEY=... OS_SECRET_KEY=...

./t-cloud-csi-conformance \
  -profile=evaluation \
  -approved-project-id="$OS_PROJECT_ID" \
  -volume-type=SSD \
  -report-path=./report.txt
```

Progress goes to standard error as each selected check starts and finishes. The readable report goes to
standard output or to `-report-path`. One JSON evidence record goes only to
`./conformance-<run identifier>.json` or to `-evidence-path`; standard output is not a JSON stream.

## Settings

A flag wins over its variable. A variable wins over the selected profile's default. The profile
itself is required as a flag and has no environment alternative.

| Flag | Variable | Purpose |
|---|---|---|
| `-profile` | | Required. `evaluation` selects every attachment-handoff, volume-lifecycle and role-startup check. `proof` selects all 29 catalogue entries. |
| `-approved-project-id` | `CSI_E2E_APPROVED_PROJECT_ID` | Required. Your assertion that this project may be mutated. Must equal `OS_PROJECT_ID`. |
| `-volume-type` | `CSI_E2E_VOLUME_TYPE` | Required. The volume type every fixture provisions. |
| `-driver-binary` | `CSI_E2E_DRIVER_BINARY` | The driver binary to drive. Defaults to `./t-cloud-csi-driver`. |
| `-report-path` | `CSI_E2E_REPORT_PATH` | Write the readable report here instead of standard output. |
| `-evidence-path` | `CSI_E2E_EVIDENCE_PATH` | Write the one JSON evidence record here instead of `./conformance-<run identifier>.json`. |
| `-paging-volume-count` | `CSI_E2E_PAGING_VOLUME_COUNT` | Proof profile only. Authorize the later-page check with a value greater than the 100-volume discovery page size. It costs that many volumes at the minimum billable size, so it is off by default. |
| `-time-budget` | `CSI_E2E_TIME_BUDGET` | Stop the run after this much wall clock and report the rest as not reached. |
| `-teardown-budget` | `CSI_E2E_TEARDOWN_BUDGET` | How long reclamation may take, separately from the bound above. |
| `-strict` | `CSI_E2E_STRICT` | Fail the run when a check the profile selected was not reached. |
| `-list-checks` | | Print the checks and exit, reaching no cloud. |
| `-help-all` | | Also print the test framework's own flags. |

Credentials are read from the process environment only:

| Variable | Required |
|---|---|
| `OS_AUTH_URL`, `OS_ACCESS_KEY`, `OS_SECRET_KEY`, `OS_PROJECT_ID`, `OS_REGION_NAME` | yes |
| `OS_SECURITY_TOKEN` | no, for temporary credentials |

No credential is accepted as a flag. A run refuses to start if one appears on its own command line. It
fails if one turns up in anything it wrote.

The binary refuses test-framework selection, skipping, repetition, shuffling, fail-fast and timeout
controls. Do not pass non-default `-test.run`, `-test.skip`, `-test.count`, `-test.shuffle`,
`-test.failfast` or `-test.timeout`; selection belongs to `-profile`. Repetition or premature exit
could duplicate effects or bypass reclamation. Use `-time-budget` for the run's own bound.

## Reading the report

Every check ends in one of three outcomes:

- **demonstrated**: the run forced the condition and the driver behaved as the check requires.
- **failed**: the run forced the condition and the driver did not.
- **not reached**: the check did not run, with a reason that tells you whether it could have. `cost not
  authorized` means a setting would enable it. `timing window missed` and `instance shape` mean the
  service or the instance did not offer the condition this time. A second run may differ. `not
  forceable here` means neither the service nor any caller can produce that state on demand, so no run of
  this binary will ever demonstrate it. `blocked by an earlier failure` and `time budget exhausted` mean
  the run stopped short. `never executed` and `unclassified skip` point at a defect in this asset rather
  than at the driver. The report says so.

A check that never ran is reported as not reached rather than omitted, so the counts always add up to
the selected list and a passing summary cannot hide a gap. A never-executed check, an unclassified skip,
catalogue incoherence or incomplete required output cannot return exit 0, regardless of strict mode.
Every not-forceable entry names the offline transport route that proves it in the catalogue, report and
evidence record.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | Every check this run could force was demonstrated. |
| 1 | A check failed, a resource survived reclamation, the catalogue was incoherent or required output was incomplete. |
| 2 | Refused before anything was created, usually a setting or a missing credential. |
| 3 | Nothing failed, but the run did not cover everything it selected. |

## What a passing run does not tell you

The report restates this. It matters for what you conclude:

- It used the instance's own kernel, udev and filesystem utilities, not the versions inside the image a
  release ships. The report names the versions it used.
- It covered one availability zone, one volume type and one instance shape.
- It drove the driver's own service surface directly. No orchestrator, node agent or sidecar took part,
  so it says nothing about scheduling, claim-driven provisioning or restart recovery.
- It says nothing about capacity, quota, throughput or latency.
- It is an observation about these builds, in this project, at this time.

## If something fails

Keep the report and the machine-readable record. The record carries the run identifier, the server-issued
identifiers the checks asserted against, the observed device paths and link names and the guest tool
versions, which is what makes a disagreement about what the service answered settleable. Both are written
through the driver's own redaction and through the run's own exact-value masking of the credentials it
holds, checked in full before either destination is opened and required to finish successfully. The
report and evidence paths must be distinct, so neither artifact can overwrite or mix with the other.
