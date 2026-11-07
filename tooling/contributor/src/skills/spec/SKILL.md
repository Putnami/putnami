---
name: spec
description: Define an outcome and acceptance in the spec the owning feature reads, native or external, with proportional independent challenge
---

# Spec

Clarify what observable outcome the request needs and how a consumer will
recognize it. Use the owning project's feature declaration and the spec it
reads as the authoritative requirement inventory. Reuse an existing current
spec for a settled correction; do not create a document to satisfy a stage
count.

From the consumer workspace root, select the CLI again in each shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read repository instructions and the accepted mandate. Discover with Putnami
first, then inspect the feature, its spec, decisions, source projects and
applicable recipe. Follow [build](../build/SKILL.md) for ownership, authority
and progress, and its [workflow contract](../../../.agents/skills/build/references/workflow.md)
for exact versioned references. Planning alone is read-only unless this
invocation authorizes editing the feature's spec. `spec` grants no task
creation, publication, merge or delivery authority.

## Workspace first

This skill also runs from your home directory (`~/.claude/skills`,
`~/.agents/skills`), where `putnami extensions install --user
@putnami/contributor` installs it. Before anything else, look for
`putnami.workspace.json` in the current directory and each directory above
it.

- Found, and this file does not lie under that workspace root: the workspace
  pins its own copy. Read `<root>/.claude/skills/spec/SKILL.md` (Codex:
  `<root>/.agents/skills/spec/SKILL.md`) and follow it instead of this file.
  When the workspace has neither, say `This workspace does not install the
  spec skill: it opts into agent content with extension:@putnami/contributor in
  agentArtifacts.` and stop.
- Not found: follow [Without a workspace](#without-a-workspace). Never create
  `putnami.workspace.json`, and never ask the person to create it.

## Find the spec the feature reads

Run `"$PUTNAMI_CLI" specs validate`, then
`"$PUTNAMI_CLI" specs inspect <feature-id>`, which names the spec in
`source.path`. A feature's entry in `putnami.features.json` either has no
`spec` member, and its spec is the native `specs/*.json` file, or names an
external spec as `"spec": {"format": "<format>", "path": "<path>"}`. An
external spec is written in a market format: `openspec`, `speckit`, `kiro` or
`gherkin`. Its `path` is relative to the workspace root, and the external spec
is then the one home of the feature's outcomes, non-goals and requirement
texts. A `features.spec_format_not_enabled` finding means the project must
list the format in `options.sdd.specFormats`; it never means writing a native
spec.

`source.path` is the manifest's `path`, a file or a folder. In a folder, edit
only the file the format reads: `spec.md` for `openspec` and `speckit`,
`requirements.md` for `kiro`, the `.feature` files for `gherkin`. Other files
beside it, such as `plan.md`, `tasks.md` or `design.md`, are not the spec.

For an external spec:

- Edit that file in its own format, and keep its headings, numbering and
  sections as the team writes them. Do not create a native `specs/*.json` for
  the feature: a feature with both fails `specs validate`.
- Give every requirement you create or renumber an anchor, a stable ID on the
  line directly above it: `<!-- putnami:id <segment-id> -->` in Markdown, the
  tag `@putnami-id:<segment-id>` above the `Rule:` in a Gherkin file. The ID
  follows the native segment grammar. A positional ID such as `FR-001` or
  `1.1` changes when the list moves, and `enforce` fails a positional
  requirement without an anchor. Keep an existing anchor when you reword a
  requirement: a changed ID is a removed requirement for the baseline. In a
  Kiro file each acceptance criterion is one requirement: anchor every
  criterion, because an anchor on its `### Requirement N` group does not pin
  the criteria below it.
- Keep checks, thresholds and windows in `putnami.features.json`. A scenario
  in the file is compared with the declared checks and never adds or removes
  one. Each format joins a scenario to its check in its own way: in `speckit`,
  put the check's anchor on the line above the scenario; in `openspec`, the
  check is the normalized `#### Scenario:` name, and an anchor there is an
  error. `kiro` has no scenarios.
- Do not restate derived facts, such as status, dates, branches, routes or
  entities, and keep each requirement text within 512 bytes.
- Propose `specs init <feature> --from <format>:<path>`, the rewrite to a
  native spec, only when the owner asks for it.

Run `"$PUTNAMI_CLI" specs validate` after the edit; a finding names its line as
`<path>#L<line>`.

## Without a workspace

A repository with no `putnami.workspace.json` keeps its specs in its own format,
and `putnami observe` reads them. It needs the observe extension in your user
scope (`putnami extensions install --user @putnami/observe`); when the command
is missing, say so in one line and stop.

1. Run `putnami observe --base <default branch> --no-fetch --output=json` from
   the repository and read its `specs`: each spec's `format` (`speckit`,
   `openspec`, `kiro` or `gherkin`), `path`, `file`, `component` and
   `requirements` (`id`, `text`, `line`). An `observe.spec_unreadable`
   diagnostic names a spec its reader refuses: fix it before you add to it.
2. Pick the spec of the component the outcome belongs to. Edit `file`, in its
   own format and the conventions of the files beside it, with the rules of
   [an external spec](#find-the-spec-the-feature-reads): an anchor above each
   requirement you create or renumber, an existing anchor kept, no derived
   facts, each text within 512 bytes.
3. When no spec covers the outcome, create one in the format the repository
   already uses, beside its other specs: `specs/<NNN>-<name>/spec.md` for Spec
   Kit, the next free number; `openspec/specs/<capability>/spec.md` for
   OpenSpec; `.kiro/specs/<name>/requirements.md` for Kiro; a `.feature` file
   beside the others for Gherkin. With no spec at all, ask the owner which
   format to start; never pick the native JSON format.
4. Run `putnami observe` again and check that `specs` lists your requirements
   with the IDs you anchored, and that no new diagnostic names your file.

Without a workspace there is no `putnami.features.json`, no native spec, no
`specs validate` and no baseline: never create them, and never ask the person
to create `putnami.workspace.json`.

## Define the outcome

State the requested outcome, in-scope and later requirements, acceptance
scenarios, explicit non-goals and unknown decisions. Keep a requested but
deferred requirement visible with its actual scope authority. Name the smallest
useful delivery unit and its first inspectable result; keep the next cycle
precise. The spec's requirement IDs and the feature-declared verification
checks are the source of truth; do not duplicate them in a local list of
expected checks. Existing feature/spec/ADR schemas stay closed. If a
requirement cannot be checked by the native producer, record the actual
acceptance evidence needed instead of declaring it verified.

For a material new or changed proposal, request an independent
[review](../review/SKILL.md) of the exact spec and relevant context versions.
Preserve its original report, findings and resolution references. A small
settled correction can reuse current agreement when its examined inputs still
match. Return the spec's source path/version, acceptance behavior, open
questions, challenge result and the next result/action. A spec is intent, not
implementation or producer proof.
