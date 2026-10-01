# CLI telemetry Legitimate Interests Assessment

**Version:** 1.0

**Applies to:** Putnami CLI anonymous usage telemetry.

**Review trigger:** Any change to the event or attribute allowlist requires a
review of this assessment. The vocabulary-guard tests are the engineering
tripwire for that review.

## Purpose test

Putnami uses the aggregated signals to improve its own CLI: identify commonly
used commands, understand project and job-scale ranges, detect broad failure
categories, and measure command duration trends. The information is not used to
advertise, profile users, make automated decisions, or observe an application
built with Putnami.

## Necessity test

Each field maps to a concrete product question:

| Field | Decision it informs |
|-------|---------------------|
| Closed command names | Which CLI workflows warrant maintenance and documentation investment |
| Project and job counts | Which workspace sizes and plans need performance work |
| Flag presence | Which supported execution modes are actively used, without retaining flag values |
| Success and failure category | Whether a broad class of invocation, authentication, API, or execution failures regresses |
| Duration | Whether a release changes the CLI's end-to-end performance profile |
| Interactive marker | Whether human and automation use exhibit materially different behavior |
| CLI version, OS, and architecture | Whether a release or platform-specific regression needs attention |
| Monthly random ID | Aggregate deduplication within a short rotation window, without a persistent identifier |

The design excludes code, file paths, workspace/project names, configuration,
environment values, error messages, user identity, and IP addresses. Those data
are not necessary for the stated purpose.

## Balancing test

The field set is limited by a compiled allowlist, and the random identifier
rotates every UTC month. The device ID and local buffer are deleted by `putnami
telemetry off`; `DO_NOT_TRACK=1`, `PUTNAMI_TELEMETRY=off`, and CI provide further
opt-outs or safeguards. A visible interactive notice is persisted before a
previous buffer can be sent, and non-TTY runs are silent until that notice exists
on the machine.

The managed receiver is hosted in `europe-west1` (Belgium). Sanitized raw
events expire from their dedicated Google Cloud Logging bucket after 1 day;
Cloud SQL retains aggregate product counts plus a 35-day privacy-control
membership projection that associates closed cohorts with monthly rotating
device IDs solely to enforce the five-distinct-contributor threshold. It
contains no raw events, and neither identifiers nor membership rows are exposed
by the read API. Database-scheduled expiry is independent of receiver instance
uptime and retains only the current UTC day plus the preceding 34 days. These
operational controls, together with the fail-silent
delivery path, limit the impact on users who do not opt out before a later
eligible run sends their buffer.
