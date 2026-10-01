# Governance

This document records who decides what in this repository, how those decisions
are made, and how a rule gets waived. It is the project's own governance. It
does not restate the product promise.

| For | Read |
|---|---|
| What is supported, on which platforms, under which license | [First Public-Release Contract](README.md#first-public-release-contract) |
| Which artifact versions a build reads, and what migrates | [Compatibility and Migration](tooling/cli/doc/21-compatibility-and-migration.md) |
| Release cadence, the release checklist, rollback | [RELEASING.md](RELEASING.md) |
| Reporting a vulnerability and what happens next | [SECURITY.md](SECURITY.md) |
| Building, testing, and opening a pull request | [CONTRIBUTING.md](CONTRIBUTING.md) |

## Roles

| Role | Who holds it | Accountable for |
|---|---|---|
| Release owner | The Licensor named in the copyright notice of [LICENSE.md](LICENSE.md), who also owns the GitHub repository | Approving a release, a rollback, a support-status change, and an exception |
| Maintainer | Anyone with write access to the repository | Reviewing and merging changes, and stopping a release |
| Contributor | Anyone who opens an issue or a pull request | Their own change and the evidence that it works |

Today one person holds every role. The roles are still written down separately,
because the point of the record is not to describe the current headcount: it is
to say which hat a decision is made under, so a delegation later is a one-line
edit to this table instead of a renegotiation.

The release owner is identified by an existing repository record — the copyright
notice — rather than by a name repeated here, so this document cannot drift from
the license.

### Delegation, absence, and failing closed

The release owner may delegate any of the decisions below by adding the
delegate and the scope of the delegation to the roles table in a pull request.
A delegation is public, revocable the same way, and takes effect when it is
merged.

**Absence is never approval.** When no approver is available, the decision does
not happen: the release does not ship, the status does not change, the exception
does not apply. Any maintainer may stop a release in progress without approval,
because stopping restores the state the project was already in.

## How decisions are made

| Decision | Approver | Where it is recorded |
|---|---|---|
| An ordinary change | Any maintainer, on the pull request | The pull request, with a green `putnami lint,test,build` gate |
| Cutting a release | Release owner | The completed [release checklist](RELEASING.md#release-checklist) in the release notes |
| Rolling a release back | Release owner; any maintainer may stop one in progress | The release notes of the superseding release |
| Promoting or demoting a support status | Release owner | The entry in [`putnami.support.json`](putnami.support.json) |
| Waiving a rule | Release owner | The register that owns the rule, listed below |
| Changing the license | The Licensor only | [LICENSE.md](LICENSE.md) |

Two rules apply to all of them:

- A decision that is not recorded did not happen. There is no verbal approval.
- A decision that weakens a gate carries an end — an expiry date, or a count
  that may only shrink. A weakening with no end is a rule change, and a rule
  change is edited in the open rather than waived in the dark.

## The exception path

An exception is a **committed, owned, reasoned record with an end**, written in
the register that owns the rule it waives. It is never a verbal decision, a
skipped step, or a disabled test.

| Register | What it waives | An entry carries | How it ends |
|---|---|---|---|
| `doctor.waivers.json` at the workspace root | A `putnami doctor` readiness finding | `code`, optional `project` and `field`, `owner`, `reason`, `expires` | The `expires` date passes. The waiver stops suppressing, the finding returns at full severity, and the stale entry itself becomes a `doctor.waiver_expired` finding |
| [`public-cut-baseline.tsv`](tooling/cli/internal/cli/testdata/public-cut-baseline.tsv) | Reviewed debt already in the tree before the public repository cut | Category, path, rule, exact evidence, count, review reason | The count may only shrink. A grown count fails as new debt, and a count that no longer matches the tree fails as stale |
| [`public-cut-allowlist.tsv`](tooling/cli/internal/cli/testdata/public-cut-allowlist.tsv) | One exact, reviewed false positive | The same six fields | The entry becomes unused and fails as stale |

Three properties are deliberate:

- **The register belongs to the rule, not to the person.** A waiver is reviewed
  in the same pull request as the code it protects, by the same people.
- **Some rules have no exception path at all.** A high-confidence secret in the
  public-cut scan is unwaivable: neither register can suppress it. When a rule is
  worth having only if it is absolute, the register refuses the entry rather than
  trusting a reviewer to refuse it.
- **A rule with no register has no exception.** If a release needs one anyway,
  the exception is recorded in the release notes with the approver, the reason,
  and the issue that removes it, and it expires at the next release. Recording it
  there is the price of not having a register.

## Support status: promotion and demotion

Support status is a public product commitment with three values — `stable`,
`preview`, `experimental`. The vocabulary, the evidence each status requires,
and the promotion and demotion mechanics are specified by the protocol that owns
the catalog:
[`protocols/support`](protocols/support/README.md#promotion-and-demotion).

Governance adds only who decides and where the decision lives:

- The **release owner** approves every promotion and every demotion.
- The record is the entry in [`putnami.support.json`](putnami.support.json),
  which is the machine-readable authority. The rationale belongs in the pull
  request that changes it.
- A **demotion needs no notice period.** It corrects a promise the project can
  no longer keep, and delaying it keeps users relying on the wrong promise.
- Python (`@putnami/python`, and the `python-server` and `python-library`
  templates) stays `experimental`, non-default, and without a parity commitment
  for the first public release. Promoting it takes the same evidence as anything
  else; no volume of hand-written prose substitutes for it.

## Licensing

[LICENSE.md](LICENSE.md) controls, today and always. This section says who may
change it, not what it says.

Two things are often confused, and only one of them is a decision:

- **The future license already granted.** FSL-1.1-MIT itself grants an
  additional MIT license for each version, effective on the second anniversary
  of the date that version was made available. Nobody decides that, nobody can
  revoke it, and it applies version by version.
- **The intent to publish v1.0.0 under MIT.** This is an intent, recorded in the
  [First Public-Release Contract](README.md#first-public-release-contract). It
  becomes real only when the Licensor replaces `LICENSE.md` in a merged pull
  request and a release ships under it. Until then, the license that applies is
  the one in the file.

Neither changes the license of a version already released. Contributions are
licensed inbound under the repository's license, as stated in
[CONTRIBUTING.md](CONTRIBUTING.md#license).

## Roadmap

The roadmap is public: it is the open issues and milestones on the repository,
and the `Product Direction` section of the [README](README.md#product-direction).

The roadmap is not a promise. What the project commits to is exactly what the
First Public-Release Contract and the support catalog state. An issue that is
open, planned, or being worked on carries no support status and no date.

## Changing this document

Open a pull request. It is approved by the release owner, like any other
governance decision.

Durable rationale — why the release owner is a role bound to the license, why an
exception must expire, and why the contributor path is proved by a test rather
than asserted — is recorded in
[ADR 0011](tooling/cli/doc/adr/0011-release-governance-and-neutral-ci.md).
Reversing one of those decisions gets a new ADR that supersedes it, rather than
a quiet edit here.
