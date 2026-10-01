# ADR 0030 — Settled decisions live in registries `validate` reads, next to the code they bind

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/features` (the `decisions.json` wire),
  `@putnami/sdd` (the `decisions-validate` step of `validate-workspace`),
  `@putnami/cli` (`internal/commands/agentctx`: the generated guidance block
  and the `putnami.context` payload)

## Context

A decision recorded only as prose does not hold. An agent reads the code it
changes, not the records beside it, and a reversal invisible in a diff passes
review. Measured on this repository, a settled pull-request rule held in at
most 9% of later pull requests, and a silently dropped cost posture cost about
500 $/month instead of 25 $.

One registry for the whole workspace fixes the reading problem and creates two
others: every agent pays for every decision in its context, and every team
settling a decision edits the same file.

## Decision

A workspace records its settled decisions in committed `decisions.json`
registries. A registry's scope is its directory:

- the registry at the workspace root holds the decisions every project follows;
- the registry in a project directory holds the decisions of that project;
- a registry anywhere else governs nothing and is not read.

A decision that binds several projects goes to the root. Every path a registry
names (its check globs and its `adr` link) is relative to its directory and
cannot leave it. Ids are unique across all registries of a workspace, because
failure messages and pull requests cite an id.

`validate-workspace` runs `decisions-validate`. It reads every registry over
the current worktree, proves every entry that carries a check, and fails
naming the decision: id, statement, settled date, and the registry to edit.

Each decision reaches an agent where it binds:

- `putnami context generate` renders every root decision into the `AGENTS.md`
  guidance block and names each project registry with its decision count;
- the `putnami.context` MCP tool returns a project's own decisions with that
  project.

The boundaries:

- **The format lives in Putnami; the content lives in each repository.**
  `protocols/features` owns the wire, the check semantics, and the strict
  reader. An absent registry is adoption, not a failure. This follows the
  `specs.baseline.json` split ([ADR 0015](0015-specs-ratchet-keeps-its-own-discovery.md)).
- **The task is uncacheable.** A check reads whatever its authored `files`
  globs name, which no static task input can cover, and an under-declared key
  serves a stale verdict while claiming to have checked. The task walks the
  worktree at most once per run, skips generated, vendored, and dot
  directories, bounds each check's matched set, and does not walk at all when
  every decision is review-only.
- **One check kind.** `json-value` asserts that a JSON Pointer in every file a
  glob set names is (or is not) a value. An entry the check cannot express is
  marked `reviewOnly`: carried to the agent, counted, never failing a build.
  An entry carries exactly one of a check or `reviewOnly`.
- **No verification-mode knob.** A written, dated decision is held or
  re-decided; it is not adopted gradually like an architecture graph.
- **Nothing is truncated.** The protocol bounds a registry to 256 entries, so
  what an agent reads stays finite without hiding a settled decision.
- **An ADR is justification only.** The registry carries the enforceable
  statement and the ADR explains why. Neither restates the other.

## Rejected alternatives

- **Parse the ADRs.** Prose carries no machine-readable claim; a gate that
  infers rules fails builds for reasons nobody wrote.
- **A lint rule per decision.** Each decision would need code and a tool
  release; a registry makes settling and enforcing one commit.
- **One registry per workspace.** Context and conflict costs grow with the
  number of decisions, not with the work at hand.
- **One file per decision.** Removes conflicts, keeps the context cost.
- **Registries in any directory.** A non-project scope has no owner and no
  `putnami.context` entry to route its decisions through.
- **A `report` mode.** An enforced decision that does not fail is the silent
  re-decision this record exists to end.
- **A richer check vocabulary up front.** A rule with no decision to enforce
  cannot be validated against a real failure.

## Consequences

- Changing a settled value means editing its registry in the same pull request,
  which puts the re-decision in the diff.
- An agent on one project reads the root decisions and that project's, not
  every project's. The guidance still names every project registry.
- A check with a wide glob costs an uncached worktree walk on every
  `validate-workspace`.
- The generated guidance block differs per workspace; its begin marker carries
  the body digest, so block ownership is unchanged.
- A second check kind is a reviewed wire change to `protocols/features`: a new
  `protocolVersion`, or a larger `kind` vocabulary with readers that refuse an
  unknown kind.
