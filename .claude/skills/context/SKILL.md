---
name: context
description: Answer what a change touches, from the workspace or, without one, from putnami observe
allowed-tools: Bash, Read, Grep, Glob
argument-hint: "[<path>|<question>]"
---

# Context

Answer "what does this change touch?" from what the tools compute, never from
a guess over file names. Name the components the change edits, those that
depend on them, the tests and coverage that speak for them, and the specs they
keep. Reading only: this skill edits nothing.

## Workspace first

This skill also runs from your home directory (`~/.claude/skills`,
`~/.agents/skills`), where `putnami extensions install --user
@putnami/contributor` installs it. Before anything else, look for
`putnami.workspace.json` in the current directory and each directory above
it.

- Found, and this file does not lie under that workspace root: the workspace
  pins its own copy. Read `<root>/.claude/skills/context/SKILL.md` (Codex:
  `<root>/.agents/skills/context/SKILL.md`) and follow it instead of this file.
  When the workspace has neither, say `This workspace does not install the
  context skill: it opts into agent content with extension:@putnami/contributor
  in agentArtifacts.` and stop.
- Found, and this file lies under that workspace root: follow
  [In a workspace](#in-a-workspace).
- Not found: follow [Without a workspace](#without-a-workspace). Never create
  `putnami.workspace.json`, and never ask the person to create it.

## In a workspace

Resolve the CLI from the workspace root:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

- `"$PUTNAMI_CLI" context map` orients on the workspace, and
  `"$PUTNAMI_CLI" context pack <project>` on one project.
- `"$PUTNAMI_CLI" projects owner <path>` names the project of a file,
  `projects deps <project>` what it depends on, and
  `projects why <a> <b>` whether a change to one impacts the other.
- `"$PUTNAMI_CLI" test --impacted --dry-run` lists what the change selects,
  dependents included.
- `"$PUTNAMI_CLI" specs inspect <feature>` reads the spec a feature keeps.

## Without a workspace

`putnami observe` reads the repository's components, the Git change, the test
and coverage reports a build left, and its specs. It needs the observe
extension in your user scope (`putnami extensions install --user
@putnami/observe`); when the command is missing, say so in one line and stop.

1. Run it from the repository, against the branch the change merges into:

   ```bash
   putnami observe --base origin/main --no-fetch --output=json
   ```

   Use the repository's default branch for `--base`. Observe measures the
   commits between the merge base and `HEAD`; list uncommitted files with
   `git status --short` and place each on its component yourself, from the
   `id` (the component's directory) of `analysis.components`.
2. Read the document:
   - `changedFiles`: each `path` and its `status`.
   - `analysis.components`: each component's `id`, `name`, `ecosystem`,
     `dependsOn`, and `impact`, which is `changed` when one of its files
     changed, `dependent` when it depends on a changed component, and
     `unaffected` otherwise.
   - `analysis.tests` and `analysis.coverage`, per component, and
     `analysis.changedLineCoverage`: present only when the job, or a local
     build, wrote reports.
   - `analysis.dependencyChanges`: the packages a lockfile moved.
   - `specs`: each spec's `format`, `path`, `component`, `changed`, and its
     `requirements`.
   - `diagnostics`: what observe could not read, such as a missing report or
     an unreachable base.
3. Answer with the changed components, then the dependent ones, then the
   specs of those components and the requirements a changed spec holds, then
   the tests and coverage when reports exist. Say plainly what observe could
   not see, from its diagnostics.

To write or change a requirement in those specs, use the
[spec](../spec/SKILL.md) skill.
