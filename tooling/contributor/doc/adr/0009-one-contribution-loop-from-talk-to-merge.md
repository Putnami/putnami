# ADR 0009 — One contribution loop from talk to merge

- **Status**: accepted
- **Scope**: `@putnami/contributor` skills and agents, the `missions`
  contract, and this repository's issue labels

## Context

Measured on 509 agent sessions over the 30 days to 27 October 2026 and the
607 pull requests they opened:

- `build` ran in 229 sessions; the six other skills were invoked by name
  in 11.
- Subagents spent 75 % of the tokens. A `build` session cost 51 M
  cost-weighted tokens, against 19 M for a session with no skill.
- 39 % of the pull requests saw at least one red CI commit.

The workflow record lists eight stages per unit (`intent`, `spec`, `design`,
`prototype`, `review`, `adr`, `candidate`, `handoff`), and putnami.app shows
eight different tabs. The owner cannot tell which stage a change is in, nor
which stages a 50-line fix skips.

## Decision

### 1. One loop, eight stages

A change runs talk, declare, show, code, check, prove, validate, merge. No
other list of stages exists. Deployment is CI/CD after merge, not a stage.
The app's tabs become views of the artifacts a stage produces.

### 2. Three units

An epic is one outcome that needs several issues, in dependency order. An
issue is one declared change, delivered by one pull request with no line or
time cap. A slice is one step of an issue the owner can validate, about three
hours or less.

### 3. The main session codes

The slices of one issue run in sequence in the main session. Parallel work
means several issues of an epic, each in its own session. By default only
review runs in a subagent: a subagent starts from an empty context and pays
the cache write again.

### 4. Code waits only for important decisions

Code starts without the owner's approval unless the plan spans several slices
with important design or spec decisions, or touches a sensitive zone. Then the
owner approves the study and a cost envelope first. After approval the agent
stops only when it exceeds the envelope, must change the approved design, or
contradicts an ADR.

`putnami change estimate` sizes the plan. S is the smallest measured class:
a range whose high bound stays within 90 active minutes. A change of 5 files
or fewer took 31 to 86 active minutes in the measured sessions, so a
30-minute line would leave no plan in S. An S plan starts without waiting.

A project declares its sensitive zones in SDD. For a project that declares
none, data migrations and the public API are sensitive zones.

### 5. Each fact has one place

The issue holds the intent and the labels. The mission holds the live state:
current slice, planned versus actual. The pull request description is the
archive. Screenshots, reports and analysis files live in one folder per issue
under `.putnami/`, outside Git.

### 6. Labels

An issue carries a `type/` label (enhancement, bug, infra, perf, refactor,
docs), a `priority/` label (urgent, high, normal, low) and the existing
`scope/` labels for its topic. A `workflow/<profile>` label selects a workflow
profile. P1 to P3 name the severity of a review finding, not a priority.

### 7. Profiles change on measurement

A workflow profile maps each stage to a model, an effort level, and the place
it runs. A profile becomes the default for an issue type only after five
paired runs on the same base commit. It wins when all three hold over the five
pairs: its total cost is lower, and lower in at least four of the five pairs;
its P1 plus P2 findings are not more; its rework is not more. Five pairs cannot
show a statistical difference, so the per-pair rule guards against one cheap
outlier.

Profiles live in the contributor policy, `options["@putnami/contributor"]` of
`putnami.workspace.json`, where they replace the tier labels and workers.
`decisions.json` holds statements with a check, not configuration; a decision
there names the default profile.

## Consequences

- `build` gets shorter: it no longer chooses between eight per-unit stages or
  delegates by tier.
- The `missions` contract carries the stage, the final state of an ended
  session, and its cost, so the app reads them instead of inferring them.
- `priority/p1` to `priority/p3` move to the new values once the `audit`
  skill, owned by `@putnami/intelligence`, files issues with them. The `tier/`
  labels go when workflow profiles replace the tier workers.

## Alternatives rejected

- A size class per change (small, standard, epic): the slice and the issue
  already carry the size, and a third axis was one more word to define.
- A cap of about 2,000 lines per pull request: a coherent feature regularly
  exceeds it, and the cap split features into pull requests the owner could
  not validate alone.
- Delegating each slice to a worker: the delegation was the largest cost
  measured, and slices of one issue share their context.

## Reversal

Restore the per-unit stages in the workflow record and the tier workers in
`build`. The labels stay: they describe issues, not the workflow.
