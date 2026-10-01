# ADR 0007 — The spec surface stays read-mostly and non-destructive

- **Status**: accepted
- **Scope**: `putnami specs *` and the `sdd.list_specs` / `sdd.spec_context`
  MCP tools, served by `@putnami/sdd` (`tooling/sdd-extension`)

## Context

The durable specification contract (`protocols/features`) makes a spec a
*reader*: it details exactly one authored feature, links decisions it does not
embed, and restates no derived fact. Its whole value is that a human agreed to
its text.

Two pressures push against that. Workspace-wide adoption makes a batch
generator look attractive, and agents are the highest-volume MCP caller. Both
would put text nobody agreed to into the one artifact that must be agreed to.

## Decision

### 1. Read-only commands, one non-destructive creator

`specs list`, `specs inspect`, `specs validate`, and `specs verify` only read.
`specs init` writes exactly one file that does not exist yet: it opens the
document with `O_EXCL`, so "never overwrite" is a property of the write, not a
check that could race. `--dry-run` prints the target path and the exact
canonical bytes the write would produce. `specs baseline --update` is the only
command that rewrites `specs.baseline.json`.

There is no `specs migrate`, no `--all` on `init`, and no `--force`. `init`
refuses an unknown feature and a feature that already has a spec, naming the
existing document.

No spec command writes a feature declaration, evidence, or maturity. Feature
Evidence comes only from build and test producers, and support status stays a
separate declaration from feature maturity.

### 2. The skeleton claims nothing the declaration did not state

`init` emits `protocolVersion`, `feature`, the declaration's own `outcome` as
the single intended outcome, and an empty `requirements` array. It writes no
non-goal, requirement, or decision link: each is an agreement or prose that no
declaration can derive.

### 3. Both MCP tools are read-only

`sdd.list_specs` and `sdd.spec_context` are annotated read-only and carry the
read contract meta. There is no `create_spec`: an MCP call has no confirmation
step, and a file written into someone's tree under their name is not a cheap,
reversible effect. An agent drafts spec content in a patch a human reviews.

### 4. One discovery stack, one interpretation

Discovery lives in the extension's `internal/features`: one contained reader,
one root enumeration, one set of bounded reads and path checks shared with
manifests and evidence. Parsing and validation are the protocol's `ParseSpec`,
`ValidateSpec`, and `ValidateSpecRepository`; the extension adds no second
opinion about what a valid spec is. It supplies only what the wire contract
cannot see: each document's owning project, and whether a linked decision
record exists in this worktree.

### 5. Contract violations fail; completeness gaps warn

An invalid document, an unresolvable or duplicated feature reference, an unsafe
decision path, or two specs for one feature fail `specs validate`. Completeness
findings (an authored feature with no spec, a publishable project with no
reviewed support entry or no owning feature link) are warnings under
extension-owned `specs.*` codes. `specs validate` is a step of the `validate`
job, so this split decides whether the gate stays usable: gaps are scheduled
authoring work, not a broken contract. An unreadable support catalog skips the
support check with one explicit warning.

`specs list` and `sdd.list_specs` never fail on document content: a broken
document is listed and marked invalid, so one bad file does not hide every
healthy spec. `inspect` does not degrade, because the caller named one exact
feature and a partial answer would be a wrong one.

### 6. Authored identity comes only from committed manifests

The feature catalog a spec resolves against comes from committed
`putnami.features.json` files, never from generated design graphs. A verdict
that flips depending on whether the workspace was built is not reviewable, and
`inspect` stays cheap because it loads no graph.

### 7. Select by owning project, never by filename

`specs list`, `specs validate`, and `sdd.list_specs` take the normal selection
vocabulary (`--projects`, `--impacted`, `--tag`, `--baseline`, ...; `projects`,
`impacted`, `baseline` on the wire), resolved by the orchestrator's canonical
filter and impact resolver before discovery. `specs inspect` and `specs init`
name an exact feature and reject selection flags: no accepted flag is silently
ignored.

Selected projects are seeds, not a wall:

- Manifests and specs are still read at every root, so "one spec per feature"
  stays exact. A duplicate spec hosted by an unselected project still fails.
- A spec for a selected feature is followed across a project boundary, with its
  source project and path preserved.
- Only what the run owns narrows: its rows, its completeness counts, and the
  findings it may fail on. A broken document outside the selection is
  attributed away.

No surface filters by filename, path, or directory. Identity is the `feature`
field; `specs list` shows the path as provenance only. The workspace's
`disable.tags` does not apply: it means "do not build here", not "has no
specs".

## Rejected alternatives

- **`specs init --all` or `specs migrate`.** Hundreds of files whose only
  content is a restated outcome, approved in bulk.
- **A write-capable MCP tool.** See decision 3.
- **Inferring the owning-feature link from the design graph.** The graph is
  build output; a clean checkout would report gaps a build silently closes.
- **Failing `specs validate` on completeness gaps.** An incomplete repository
  is not a broken one, and a gate that cannot tell them apart gets disabled.
- **Filtering the catalog after a whole-workspace read.** It bounds the output
  but not the cost that grows with the repository.

## Consequences

- Writing a spec's content stays a human act; tooling produces a valid
  skeleton and reports what is missing.
- A use case this surface cannot express changes the shared protocol, never a
  tooling-local field.
