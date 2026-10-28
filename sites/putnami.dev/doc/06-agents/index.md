# Agents

There are two different things an agent can do with your codebase.

It can **write code in** it — read some files, follow the conventions it finds,
open a diff. Most tooling supports this, because it only requires the repository
to be readable.

It can **operate** it — know what a change affects before making it, run the
right jobs, read the result as data, tell you what is not production-ready and
why. That requires the system to state what it is, in a form something other
than its author can parse.

Putnami is built for the second one. This page is the contract that makes it
possible. For the practical setup — assistant context files, prompt patterns —
see [Develop with AI assistants](/docs/how-to/develop-with-ai).

---

## Orientation: read the system, not the files

When you run `putnami init`, the workspace gets one short Putnami block in
`AGENTS.md` (which a new `CLAUDE.md` imports). That is the whole of what Putnami writes. Your own
durable rules go in `.agents/constraints.md`, a file you create yourself. The
block tells an assistant to read it when it exists, and Putnami never writes it.
Agent skills come only when you run `putnami extensions install @putnami/contributor`
(see [Develop with AI assistants](/docs/how-to/develop-with-ai#install-and-update-agent-workflows)).
Adding an extension later? `putnami context generate` refreshes the block and the skills.

That teaches an assistant your conventions. Two commands teach it your *system*.

### `putnami context map` — where things are

```bash
putnami context map                         # print the map as Markdown
putnami context map --output=json           # print the map document as data
putnami context map --projects <project-id> # print only the projects you name
```

The workspace orientation map, rebuilt from committed artifacts — project
manifests, `schema/openapi.json`, `schema/config.jsonschema.json`, READMEs, and
the docs tree. It answers "which project owns X", "what depends on Y", "where
are the APIs, the config keys, the docs" without shell exploration.

It is byte-deterministic on an unchanged tree, and nothing writes it: neither
the command nor a build stores a copy. `putnami context map` prints it, rebuilt
on every call.
`--projects` and `--impacted` narrow the printed projects.

### `putnami context pack` — how one project is composed

```bash
putnami context pack --project <project-id>
putnami context pack --output=json    # every project's document, as data
```

It prints one deterministic **agent-context document** per project and writes
no file:

- **identity and graph** — id, name, path, type, tags, languages, and the ids of
  dependencies and dependents;
- **composition roots** — the application main, the describe entrypoint, and
  other roots, each with a path and a provenance tag;
- **capabilities, contracts, infra, migrations** — references carrying a path
  and a `sha256:` digest, pointing at the committed artifacts;
- **representative sources** — ordered ranges (`path`, `startLine`, `endLine`, a
  reason, a token estimate). Ranges only;
- **tests** — referenced conformance packs and fixture digests — or, when
  there are none, a machine-readable reason for their absence;
- **docs** — adjacent documentation, each marked checked or unchecked;
- **config** — a config-schema reference and refs-only hints about the
  operational surface;
- **provenance** — the workspace revision, the generator, and the aggregation
  method.

Three properties are worth stating plainly, because they are what make the
document safe to hand to a tool you don't control.

**It aggregates by reference.** Paths, digests, and line ranges. Never file
content. The types have no slot for content, and the safety gate rejects any
string long enough to look like it anyway.

**It is never stored.** Every read builds it again from the working tree, so no
copy can go stale. It embeds the workspace revision and content digests, so a
stored copy would churn on every commit. Determinism is pinned byte-for-byte by
tests at a fixed tree.

**Publication fails closed.** Before a document can leave the workspace for an
authorized index, a redaction gate runs and reports as hard errors: a reference
to a sensitive path that is not flagged sensitive, anything resembling embedded
content, any path that is absolute or escapes the workspace, and any duplicated
reference. An empty document fails the gate, so it can never pass vacuously.

You can adjust the document, narrowly. `<project>/schema/agent-context.overrides.json`
adds or removes representative-source and doc entries and force-flags paths as
sensitive. It cannot override identity, capabilities, or provenance — those are
framework-owned facts, and an author who could rewrite them could make the
document lie.

---

## Through the CLI

An agent reaches the workspace with the commands a person runs. Add
`--output=jsonl` to a query for structured output.

| | Commands | What they answer |
| --- | --- | --- |
| **Orient** | `putnami projects list` · `putnami projects describe` · `putnami context pack` · `putnami context map` | What is in this workspace, how is this project composed, where should I read first |
| **Blast radius** | `putnami projects owner` · `putnami projects deps` · `putnami projects why` · `putnami <verb> --impacted --dry-run` | Which project owns this path, what depends on what, what does my change affect, and by what path |
| **Intent** | `putnami features` · `putnami specs` (from `@putnami/sdd`) | What was this built for, what were the non-goals, what did the team agree to |
| **Execute** | `putnami lint` · `putnami test` · `putnami build` | Run the jobs, then read every failure as `file:line:col` with severity and code |

Putnami serves no protocol server and writes no agent-host registration. An
earlier Putnami wrote a `putnami` entry to `.mcp.json`; `putnami install` and
`putnami upgrade` remove it when it is the entry Putnami wrote.

### Graph answers read the recorded workspace view

Project identity and dependency edges belong to the language extensions. The CLI
keeps a local copy in `.putnami/workspace-index.json`. When the copy is missing,
the graph commands say so and point at `putnami projects sync`, because an empty
graph would read as *"there is nothing here"* rather than *"nobody has told me
yet"*.

### A preview before a run

`--dry-run` returns the selected dependency plan without starting a subprocess.
Pair it with `--impacted` to see what a change would run before it runs.

---

## Blast radius before action

```bash
putnami lint,test,build --impacted
```

`--impacted` resolves the projects affected by the current changes against a
baseline, transitively through the dependency graph. The baseline is resolved in
a fixed order — workspace config, nearest configured epic branch, trunk, local
main or master, then the upstream tracking ref — and never the current branch
itself, so it cannot quietly resolve to "nothing changed".

Run it once, before declaring the change complete. It also runs every project
that depends on the change, so while iterating, select only the projects you
changed with `--projects <a>,<b>`. The generated `AGENTS.md` gives agents the
same rule.

When an agent needs to justify the result rather than trust it,
`putnami projects why <from> <to>` prints the shortest path from the changed
project to the impacted one over the same edges, with each edge's kind and the
task scope, and `putnami projects owner <path>` maps a file path back to the
project that owns it using the same logic.

### Deliver in gated phases, not in small pull requests

An `--impacted` run ends with the size of the change, split into authored code,
tests, docs and generated files. The size is information: no line count fails
the run. A change that updates its tests and docs is larger by design, and
agent work, such as an epic, is often large and legitimate.

What keeps a large change safe is how it lands:

1. One pull request carries one intent.
2. Work that spans several projects goes on an integration branch listed in the
   workspace `epicBranches`, one phase per commit.
3. Every phase passes the gate before the next one starts.
4. The branch merges as one unit that one revert undoes.

When the authored code of a change spans projects that no dependency relates,
and the branch is not on an epic branch, the run prints a mixed-intent warning.
The warning never changes the exit code. Both are also recorded as a
`selection:change-shape` session event. The managed `build` workflow follows
the same rule and preserves an allocated workspace branch. Delivery units follow observable
value, compatibility, release and rollback boundaries. A project, worker or
task count does not establish a separate unit or proposal. Intermediate units
retain their parent's pending requirements and use non-closing references.

---

## A surface that parses the same way every time

```bash
putnami build --output=json
putnami test  --output=jsonl
```

Exit codes, output modes, the reserved global-flag registry, and the machine
documents are the [`cli` protocol](/docs/protocols) — versioned, and validated
by one conformance corpus executed from both the Go and the TypeScript
implementation. An agent that learned the shape does not have to relearn it
because a release changed a log line.

Every job also emits one JSONL event stream — logs, progress, phases,
diagnostics, metrics, artifacts, results — so the agent watching a build and the
human reading the log are parsing the same lines.

---

## Verifying the work

Generating a change is the easy half. The useful half is telling whether it is
safe.

```bash
putnami doctor --profile production
```

`doctor` is a read-only production-readiness preflight. It derives findings from
committed manifests — incomplete capabilities, missing required config, invalid
committed schemas, config shadowing — and grades them by deployment profile.
Under `--profile production` a high or critical finding exits `2`; `dev` and
`test` stay advisory. **Config values are never read or emitted**, so running it
in a pipeline leaks nothing.

Each check carries a remediation baked into the check itself, and the code
taxonomy is frozen. Findings you have consciously accepted go into a committed
`doctor.waivers.json` — a waiver is a reviewable decision in git, not a flag
someone remembered to pass.

Alongside it: conformance packs test declared capabilities against shared
fixtures, and the orientation document is rebuilt on every read, so it cannot
go stale behind the code.

---

## The documentation is part of the surface

Stable URL prefixes per section, **Copy as Markdown** on every page, and
[`/llms.txt`](/llms.txt) as a machine-readable index of the whole tree. The
structure an agent reads is the structure you read.

---

## What this deliberately does not do

- The agent-context document is **per project**, and it references rather than
  embeds. It is an orientation artifact, not an index of your repository — an
  agent still reads the files it decides to read.
- Author overrides are narrow on purpose. If you could rewrite identity or
  capabilities, the document could lie, and every guarantee above would be
  worth nothing.
- None of this makes an agent correct.

That last one matters. What the contract buys you is that an agent's actions are
**bounded, inspectable, and reversible** — which is the precondition for letting
one run at all.

---

## Read next

- [Protocols](/docs/protocols) — the contracts underneath everything on this page.
- [Develop with AI assistants](/docs/how-to/develop-with-ai) — the practical setup and prompt patterns.
- [Write a feature spec](/docs/how-to/write-a-feature-spec) — how intent gets recorded so `sdd.spec_context` has something to return.
- [Spec-driven development](/docs/concepts/spec-driven-development) — the loop that turns declared intent into a CI verdict, and the architecture contracts (ARC and DARC) that bound what agents may couple.
- [Principles](/docs/principles) — in particular, *automation is a first-class user*.
