# ADR 0006 — The spec source contract

- **Status**: proposed; the owner accepts it by merging its proposal
- **Scope**: `go.putnami.dev/protocol/features` (`protocols/features`) and
  `@putnami/sdd` (`tooling/sdd-extension`)
- **Implements**:
  [ADR 0004 of `@putnami/sdd`](../../../../tooling/sdd-extension/doc/adr/0004-a-spec-may-live-in-an-external-market-format.md),
  decisions D-039 to D-047. It records D-048 to D-051 of the root
  `decisions.json`, which build sessions took and the owner has not reviewed
- **One-way door**: the `spec` reference is authored in committed manifests,
  and the format names are public wire values. A release can add to them but
  cannot change them without a new manifest version and a move of every
  manifest that uses them.

## Context

ADR 0004 of `@putnami/sdd` lets an external file hold a feature's prose. It
leaves four things to this contract: the shape of the manifest reference, the
exact requirement ID mapping, where an anchor is written, and how a reader
plugs in. Its review left two more. A manifest can name a format its project
did not opt into. A cached spec task must key on the external files, and a
task input is a static pattern, so it cannot depend on a manifest value.

## Decision

1. **Manifest version 3 adds one optional member.** A `features[]` entry may
   carry `"spec": {"format": "<format>", "path": "<path>"}`. The object is
   closed. Versions 1 and 2 refuse the member, as version 1 refuses `surface`.
   The design graph and `Module.Feature` carry no reference.
2. **The formats are a closed list:** `openspec`, `speckit`, `kiro` and
   `gherkin`. `path` is a workspace-relative file or directory that cannot
   escape the workspace.
3. **A reader is a pure function.** `SpecReader.Read` takes the feature and the
   bytes of the files under `path`, and returns a `Spec`, one record per
   requirement and positioned findings. It uses no network, no model, no clock
   and no filesystem. `ReadSpecSource` wraps every reader, so bounds, duplicate
   IDs and positions have one implementation.
4. **A finding points into the file.** Its field is
   `<workspace path>#L<line>`.
5. **A name becomes an ID by one mapping.** `NormalizeRequirementName`
   lowercases ASCII letters, keeps ASCII digits, and turns each other run of
   ASCII characters into one hyphen. It trims the hyphens at both ends. A name
   with a non-ASCII character, with nothing left, or longer than 128 bytes after
   mapping has no ID and needs an anchor.
6. **An anchor stands alone on the line above its requirement.** It reads
   `<!-- putnami:id <segment-id> -->`. In a Gherkin file it is the tag
   `@putnami-id:<segment-id>` on the line above the `Rule:`. The ID follows the
   native segment grammar.
7. **A format a project did not opt into is an error.** `specs validate` fails
   on the manifest entry in every verification mode, and the file is not read.
   A warning would let a gate pass over prose nobody read. (D-048)
8. **`options.sdd.specFormats` resolves like the verification policy:** the
   project's list, then the workspace's, then empty. A project's list replaces
   the workspace's. A wrong type, an unknown format or a repeated format is an
   error. (D-049)
9. **A reader reads only `.md` and `.feature` files.** `specs-validate` and
   `specs-ratchet-validate` declare `git:**/*.md` and `git:**/*.feature`, and
   external files are read through the Git candidate cut, so the key holds
   every byte a reader sees. A new extension needs a new task input, and a test
   pins the two together. (D-050)
10. **The criteria projection names the format.** A `SpecCriteriaGroup` for an
    external spec carries `format`, and its `spec` is the reference path. A
    native group omits `format` and keeps the `specs/*.json` rule, so its bytes
    do not change and the projection stays version 1.
11. **Scenarios are compared only where a format has them.** A reader sets
    `Scenarios` to a list, empty or not, on a requirement its format can attach
    scenarios to. It leaves `Scenarios` nil otherwise, and that requirement is
    not compared. Kiro has no scenarios, so D-041 has nothing to compare there.
12. **Two references cannot overlap.** A path that equals, contains or lies
    inside another feature's path is an error: a spec details one feature.
13. **An unanchored positional requirement fails only in `enforce`.** In
    `report` it is projected and `specs validate` adds no finding about it.
14. **A format's scenarios may belong to the file.** A Spec Kit scenario sits
    under a user story, and nothing ties a story to a requirement. A reader of
    such a format sets `SpecReading.Scenarios` to a list, empty or not. Those
    scenarios are compared with every check the projected requirements declare.
    A mismatch either way stays a warning, as D-041 says. (D-051)
15. **An anchor above a scenario names its check.** The anchor keeps the form of
    §6. A scenario sentence does not normalize to a check ID a person would
    write, so an anchor is how an author joins the two. Without one, the
    scenario's check is its normalized text, or none.
16. **A finding can block only in `enforce`.** A reader stays independent of the
    mode. It lists such findings in `SpecReading.EnforceFindings`, as warnings.
    `SpecProjection.EnforceOnly` gives them for one mode: warnings in `report`,
    errors in `enforce`, with the unanchored requirements of §13 as errors. A
    Spec Kit `[NEEDS CLARIFICATION]` marker is the first such finding.
17. **A reader can locate an outcome or a non-goal.** `SpecReading.TextPositions`
    maps a field such as `outcomes[0]` to its line, so a validation finding on
    that text follows §4. A field with no position stays at the reference.

## Rejected alternatives

- **The `surface` rung, version 2.** Readers of the released version 2 refuse
  an unknown member, so a version 2 manifest with `spec` would fail them with
  no version to explain why.
- **Ignoring a format that is not opted in.** The feature would show no spec
  and no reason.
- **`git:**` as the whole input.** Every source edit would rerun every project's
  spec task.
- **An input derived from each manifest's `path`.** Task inputs are static.
- **`**/*.md` as an ordinary pattern.** It walks ignored directories such as
  `node_modules`, and hashes files no candidate cut can read.

## Consequences

- A workspace with no `spec` reference keeps its outputs. Its spec tasks rerun
  when any Markdown or Gherkin file changes, because the key cannot depend on
  a manifest.
- `specs-validate` keys on `**/putnami.json` and `putnami.workspace.json`,
  because it now reads the policy.
- Until a format's reader ships, naming that format is an error that says so.
- ADR 0015 of the CLI gains the new keys.
