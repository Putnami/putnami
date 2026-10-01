# ADR 0011 — Release governance: an owner bound to the license, exceptions that expire, and a contributor path proved by execution

- **Status**: accepted
- **Scope**: the workspace-root governance record (`GOVERNANCE.md`, `RELEASING.md`, `SECURITY.md`, `CONTRIBUTING.md`), `@putnami/cli` (`tooling/cli`, `tooling/cli-documents`) as the home of the gates, `protocols/support`

## Context

A release needs written answers to five questions: who approves it, how a rule
gets waived, when releases happen, who can roll one back, and how a subject
earns or loses a support status. An unnamed approver is not neutrality: it makes
the authority unreviewable and delegation a renegotiation.

An exception with no register and no end is a rule nobody enforces. And the
contributor path cannot be proved by a maintainer's run: credentials, a warm
store and a reachable registry are invisible inputs that a contributor does not
have.

## Decision

### 1. The release owner is a role bound to the license

The release owner is **the Licensor named in the copyright notice of
`LICENSE.md`**, not a name repeated in a governance file. The role approves
cutting a release, rolling one back, changing a support status, and granting an
exception.

- **The record cannot drift from the license.** Changing who the project belongs
  to changes who approves, in one place.
- **Absence is never approval.** With no approver available, the decision does
  not happen. Any maintainer may stop a release without approval, because
  stopping restores the state the project was already in.
- **Delegation is a table edit.** A delegate and their scope are added to the
  roles table in a reviewed pull request, and revoked the same way.
- **A rollback is a version pin**, per [ADR 0002](0002-cli-vnext-contracts.md)
  §4, never a compatibility flag.

### 2. An exception is a committed record with an owner, a reason, and an end

Every waiver lives in the register that owns the rule it waives, and every
register enforces its own end: a doctor waiver expires on its date and then
fails as its own finding; the public-cut baseline may only shrink; an allowlist
entry fails when it goes unused.

- **Some rules are unwaivable by construction.** Neither register accepts a
  high-confidence secret from the public-cut scan. A rule worth having only if
  it is absolute refuses the entry itself, instead of trusting a reviewer under
  deadline.
- **A rule with no register has no exception**, except a release-notes entry
  that names the approver, the reason and the issue that removes it, and expires
  at the next release. That path is deliberately more expensive than adding a
  register.

### 3. Support status moves one step at a time on evidence; demotion does not

Promotion is `experimental` → `preview` → `stable`, one step per decision. Each
step names its evidence: documentation, gate coverage, and a
[compatibility-budget](0010-compatibility-budget.md) entry for anything with a
wire format. Demotion may skip steps and takes effect at once. A promotion makes
a promise, so it pays for review; a demotion withdraws a promise the project
already knows it cannot keep.

### 4. The neutral contributor path is documented commands the tests execute

The contributor path is one fenced block in `CONTRIBUTING.md`, between
`neutral-contributor-ci` markers. `tooling/cli-documents/neutral_contributor_ci_test.go`
reads that block and runs it:

- in a workspace fixture with an empty artifact store and a lock that holds no
  installed artifacts;
- with every cloud, cache, registry and cloud-provider credential cleared and
  `HOME` redirected;
- with registry and cache endpoints pointed at a server that refuses and counts
  every request; the run must make zero;
- through the real `--impacted` selection over a real git history.

The block is the single source of truth: editing the documentation without
changing what works fails the build.

Maintainer CI is Putnami Cloud CI, configured by `putnami.ci.json`. Its
commands, the documented contributor gate, and the gate the generated guidance
derives must be the same task set, and `putnami.ci.json` declares no advisory
command, because a contributor cannot tell one apart from the gate.

`.github/workflows/contributor-ci.yml` runs the same documented commands with
no secret, because a fork's pull request cannot reach Putnami Cloud CI. It is
corroboration, not the definition, and it stays a non-required check.

### 5. Governance completeness is gated, and the gate states its own limit

The same test file asserts that the governance record answers its questions:
the roles table names the license as the source of the release owner; the
exception path names each register and its end; the checklist covers the gate,
the compatibility budget, the support catalog and the rollback plan; and
`SECURITY.md` reconciles its 72-hour triage clock with its business-day windows.
Each assertion has a non-vacuity check, per
[ADR 0002](0002-cli-vnext-contracts.md) §5.

A completeness gate checks presence and shape, never truth. Whether the approver
is right or a window is achievable is for the release checklist and human
approval.

## Consequences

- The documented contributor path runs on every CLI test run, without
  credentials, so it cannot rot silently.
- A new support status costs a documentation row: `protocols/support` pins its
  promotion table against the code's status vocabulary.
- A rule with no register cannot be waived cheaply, which pushes work toward
  registers that expire on their own.
- A single approver is a single point of failure. Absence blocks the decision
  instead of defaulting to yes, and delegation is one merged edit away.
- No gate performs the switch from the private archive to the public root; it
  stays a separately approved human operation.
