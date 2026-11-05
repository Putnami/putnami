# ADR 0004 — A spec may live in an external market format

- **Status**: accepted
- **Settled**: 2026-11-04, decisions D-039 to D-047 of the root
  `decisions.json`; D-056, taken on 2026-11-05 by a build session for the
  owner to review, details decision 8
- **Amends**:
  [ADR 0007](../../../cli/doc/adr/0007-read-mostly-spec-surface.md), §1 and
  §2: `specs init --from` drafts a native spec from an external one
- **Scope**: `@putnami/sdd` (`tooling/sdd-extension`), the spec contract of
  `protocols/features`, and every workspace that sets
  `options.sdd.specFormats`

## Context

`@putnami/sdd` reads only its own spec format: one `specs/*.json` file per
feature, parsed by `protocols/features.ParseSpec`. Many teams already write
specs in a market format. A market format is a spec layout that a
spec-driven tool outside Putnami defines:

- **OpenSpec**: one folder per capability, with `### Requirement: <name>`
  headings and `#### Scenario:` blocks.
- **GitHub Spec Kit**: a `spec.md` per feature, with numbered requirements
  such as `FR-001`.
- **Kiro**: a `requirements.md` with numbered requirements such as `1.1`,
  written in EARS. EARS is a sentence template for requirements ("WHEN
  <trigger> THE SYSTEM SHALL <response>").
- **Gherkin**: `.feature` files with `Feature:` and `Scenario:` blocks.

Those teams cannot adopt the SDD gate without rewriting every spec first.
A rewrite of that size does not happen, so they get none of the gate.

Two rules constrain any answer. First, one fact has one home: a JSON copy of
a Markdown spec would be a second home, and the two would drift. Second, the
native spec has guarantees a Markdown file lacks: a closed schema, canonical
bytes and hand-authored requirement IDs. The gate relies on stable IDs,
because `specs.baseline.json` records `feature#requirement` pairs.

## Decision

1. **An external file can be the one home of a feature's prose.** The prose
   is the outcomes, the non-goals and the requirement texts. A reader
   projects the external file in memory into `protocols/features.Spec`. No
   command commits that projection as a JSON file. (D-039)
2. **A feature exists only through its declaration.** The declaration stays
   in `putnami.features.json` or `Module.Feature`. The reference to an
   external spec is written on the feature's entry in
   `putnami.features.json`, for example
   `"spec": {"format": "openspec", "path": "openspec/specs/invoice-export"}`.
   Specs resolve against committed manifests only, never the design graph
   (ADR 0007 §6), so `Module.Feature` names no spec. A Markdown folder never
   creates a feature. D-025 is unchanged. (D-040)
3. **Executable criteria stay in the manifest.** `verification` (checks,
   thresholds, windows) is authored in `putnami.features.json`. The reader
   compares the scenarios of the external file with the declared checks. A
   check with no matching scenario is a warning, and so is a scenario with
   no matching check. Scenarios never define or shrink the check set.
   (D-041)
4. **A requirement ID survives reordering.** A name-based ID, such as the
   name in an OpenSpec `### Requirement: <name>` heading, is accepted after
   kebab normalization: lowercase words joined by hyphens, in the ID grammar
   of the native spec. The spec source contract defines the exact mapping. A
   positional ID,
   such as `FR-001` or `1.1`, needs an explicit anchor before its project can
   set `options.sdd.verification.specs` to `enforce`. An anchor is a stable
   ID the author assigns to the requirement; the spec source contract in
   `protocols/features` defines where it is written. When a baselined
   `feature#requirement` disappears, the gate fails, as it does for a native
   spec. (D-042)
5. **The reader is strict about size and lenient about layout.** It reads the
   sections it recognizes and ignores an unknown section. A section that
   restates a derived fact (status, dates, branch, routes, entities) is a
   warning. A requirement text over the 512-byte bound of the native spec is
   an error that names a split. (D-043)
6. **Only `doc/adr/*.md` links are decisions.** `design.md` and `plan.md` are
   context. The reader never projects them as decision links. (D-044)
7. **Native and external specs share one discovery.** The rule "one spec per
   feature" covers both. A feature with a native spec and an external spec
   fails `specs validate`, so a half-done migration fails the gate. (D-045)
8. **`specs init --from` drafts a native spec.**
   `specs init <feature> --from <format>:<path>` writes exactly one new
   native file with `O_EXCL`. It supports `--dry-run`. It carries only text
   copied from the source. It has no `--all` and no `--force`. There is
   still no `specs migrate`. This amends ADR 0007. (D-046) The source is the
   external spec the feature's manifest entry names, read through the
   discovery the gate uses, and the command refuses what `enforce` would
   refuse. (D-056)
9. **A project opts in to each format.** A project lists the formats it reads
   in `options.sdd.specFormats`. The default is empty, so a workspace that
   does not opt in sees no change. (D-047)

Parsing lives in `protocols/features`, next to `ParseSpec`, so there is one
interpretation of each format, as ADR 0007 §4 requires. `tasks.md` and
`plan.md` are process state, not specs, and no reader reads them.

## Rejected alternatives

- **A one-shot converter.** It rewrites every spec at once, so a reviewer
  approves hundreds of files in bulk. ADR 0007 rejects that for
  `specs migrate`. It also forces the rewrite this decision exists to avoid.
- **A committed JSON mirror.** A generated `specs/*.json` beside each
  Markdown file is a second home for the same prose. The two drift, and a
  reviewer cannot tell which one is the agreement.
- **Scenarios as the check set.** The baseline records `feature#requirement`,
  not checks. A deleted scenario would then shrink the check set and turn a
  requirement green with less proof, and no gate would see it.
- **A Markdown folder that declares a feature.** Feature identity would then
  depend on a directory layout. Under D-025, features come from
  declarations only; a hosted spec details a declared feature and never
  mints one.
- **Parsing in the extension.** A second parser outside the protocol gives a
  second opinion on what a valid spec is.

## Consequences

- A team with Spec Kit, OpenSpec, Kiro or Gherkin specs sees coverage,
  proofs and gaps on its first `report` run, with no rewrite.
- `enforce` needs anchors on positional IDs. Name-based IDs need none, but a
  renamed requirement is a removed requirement, and the gate fails until the
  baseline is updated.
- The native format stays the strongest guarantee. `specs init --from` moves
  one feature to it at a time, after a review. The pull request that adds
  the native file also drops the manifest's reference to the external spec;
  otherwise decision 7 fails it.
- `protocols/features` gains a spec source contract and the
  `{format, path}` reference on a feature declaration. That reference is
  hard to undo once released.
- An external spec file becomes an input of the cached spec tasks.
  `specs-validate` and `specs-ratchet-validate` key today on
  `**/specs/*.json` and `**/putnami.features.json`, so a changed Markdown
  spec would serve a stale verdict. The readers must declare the external
  files their opted-in formats read as task inputs, and
  [ADR 0015](../../../cli/doc/adr/0015-specs-ratchet-keeps-its-own-discovery.md) follows
  that change.
- Each reader must keep pace with its upstream format. A format change
  upstream can turn a passing spec into a warning or an error.
