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
`AGENTS.md` (which a new `CLAUDE.md` imports), the declarations of the
`@putnami/contributor` extension's agent content (its skills and agents land
once the extension is published and installed; see
[Develop with AI assistants](/docs/how-to/develop-with-ai#install-and-update-agent-workflows)),
and a `.mcp.json` entry that registers the Putnami MCP server. That is the whole
of what Putnami writes. Your own durable rules go in `.agents/constraints.md`, a
file you create yourself — the block tells an
assistant to read it when it exists, and Putnami never writes it. Adding an
extension later? `putnami context generate` refreshes the block and the skills.

That teaches an assistant your conventions. Two commands teach it your *system*.

### `putnami context map` — where things are

```bash
putnami context map            # regenerate
putnami context map --print    # render to stdout, write nothing
```

The workspace orientation map, rebuilt from committed artifacts — project
manifests, `schema/openapi.json`, `schema/config.jsonschema.json`, READMEs, and
the docs tree. It answers "which project owns X", "what depends on Y", "where
are the APIs, the config keys, the docs" without shell exploration.

It is byte-deterministic on an unchanged tree, lives gitignored under
`.putnami/context-map/`, and is never committed. `putnami build` refreshes it
automatically outside CI, so it does not go stale behind the code.

### `putnami context pack` — how one project is composed

```bash
putnami context pack --project <project-id>
```

It writes one deterministic **agent-context document** per project to
`<project>/.gen/agent-context.json`:

- **identity and graph** — id, name, path, type, tags, languages, and the ids of
  dependencies and dependents;
- **composition roots** — the application main, the describe entrypoint, and
  other roots, each with a path and a provenance tag;
- **capabilities, contracts, infra, migrations** — references carrying a path
  and a `sha256:` digest, pointing at the committed artifacts;
- **representative sources** — ordered ranges (`path`, `startLine`, `endLine`, a
  reason, a token estimate). Ranges only;
- **tests** — the policy, referenced conformance packs, and fixture digests —
  or, when there are none, a machine-readable reason for their absence;
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

**It is ephemeral.** It lives under the gitignored `.gen/` root and is never
committed — it embeds the workspace revision and content digests, so it would
churn on every commit. Determinism is pinned byte-for-byte by tests at a fixed
tree, not by committing the artifact.

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

In CI or pre-flight:

```bash
putnami context pack --check   # re-derives, exits non-zero on drift, writes nothing
```

---

## The MCP server

```bash
putnami mcp install   # writes or repairs the putnami entry in .mcp.json, merge-aware
putnami mcp           # run the server over stdio (your client spawns this)
```

`putnami init`, `putnami install` and `putnami upgrade` add the `putnami` entry
to `.mcp.json` when it is missing. Other servers and unknown keys in an existing
file are preserved, a `putnami` entry you changed is kept, and a file that does
not parse is left untouched with a warning. `mcp install` writes the same entry
on request, and is the one command that rewrites a diverged one.

The server speaks JSON-RPC 2.0 on stdin/stdout, exits with the session, and
exposes the workspace as typed tools instead of shell exploration:

| | Tools | What they answer |
| --- | --- | --- |
| **Orient** | `list_projects` · `describe_project` · `agent_context` · `workspace_map` | What is in this workspace, how is this project composed, where should I read first, which project owns X |
| **Blast radius** | `impacted` · `why_impacted` · `deps` · `find_owner` · `topo_sort` | What does my change affect, and by what path |
| **Intent** | `sdd.list_features` · `sdd.feature_context` · `sdd.list_specs` · `sdd.spec_context` | What was this built for, what were the non-goals, what did the team agree to |
| **Execute** | `run_jobs` · `get_diagnostics` | Run lint / test / build, then read every failure as `file:line:col` with severity and code |

Plus the `workspace://context` resource, so a client that prefers resources over
tool calls gets the same facts.

### Every graph answer says what it was derived from

Project identity and dependency edges belong to the language extensions. The CLI
keeps a local copy in `.putnami/workspace-index.json` so a read-only tool answers
without paying for a probe — and since that copy can be missing or old, every
graph answer carries a `workspaceView` block saying which:

```json
"workspaceView": {
  "freshnessState": "fresh",
  "observedAt": "2026-09-01T09:12:44Z",
  "probeDigest": "sha256:…",
  "ageSeconds": 3420,
  "message": "the recorded workspace view (workspace-index.json) was observed 57m ago"
}
```

Three states, and the tool behaves differently in each:

| `freshnessState` | What the tool does |
| --- | --- |
| `fresh` | Answers normally. |
| `stale` | Answers anyway, and the block says the copy is past its bound. |
| `absent` | Returns an **error result** carrying the partial answer and the block, whose message names `putnami projects sync`. |

The last row is the important one. A workspace nobody has probed used to answer
an empty-but-well-formed graph, which reads as *"there is nothing here"* rather
than *"nobody has told me yet"*. It is now a refusal an agent can act on.

This is the consumer half of a declared architecture contract
(`cli.workspace-probe-view.v1` in `tooling/cli/putnami.architecture.json`): the
freshness bound, the fail-closed-on-missing behavior and the field names above
are declared there and joined to the code by a conformance test.

### Every tool declares its own contract

Each tool carries a `putnami.dev/contract` block in its metadata: an `access`
value (`read` or `mutating`), a `readOnly` flag, whether it `supportsDryRun`,
and — when it mutates — a `mutates` list naming exactly what it touches.

**`run_jobs` is the only mutating tool.** Everything else is read-only. It runs
jobs through the same planner and engine as the CLI, declares that it mutates
the workspace and the cache, supports `dryRun` to return the selected dependency
plan without starting a subprocess, and refuses long-lived serve mode and any
job declared to mutate external systems.

An agent doesn't have to guess which call is safe. The tool says so.

### Optional request provenance

Off by default. Set `PUTNAMI_AGENT_IDENTITY=1` and explicitly set
`PUTNAMI_AGENT_MODEL` in the MCP server environment, and requests carry bounded
harness and model headers — so what an agent did is attributable after the fact,
by the same principle that makes every other action in Putnami reviewable.

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

When an agent needs to justify the result rather than trust it, `impacted`
returns a reason per project (the changed file that claimed it, or the edge
that reached it), `why_impacted` returns the shortest path from the changed
project to the impacted one over the same edges with each edge's kind, and
`find_owner` maps a file path back to the project that owns it using the same
logic.

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
`selection:change-shape` session event. The managed `plan`, `fix`, `execute`
and `epic` skills follow the same rule.

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

Alongside it: `get_diagnostics` re-reads the complete failure set from the last
executed run without rerunning the work, conformance packs test declared
capabilities against shared fixtures, and `putnami context pack --check` keeps
the orientation document from going stale behind the code.

---

## Agent readiness, marker by marker

The command and collector are [public source](https://github.com/Putnami/putnami/tree/main/intelligence/agent-readiness) in the separate
`@putnami/agent-readiness` extension:

```sh
putnami extensions install --user --latest @putnami/agent-readiness
putnami agent-readiness --print-payload
putnami agent-readiness
```

In a Putnami workspace, declare the extension in that workspace's extensions.
Print-only mode sends nothing; a normal run sends the validated payload to the
existing anonymous scoring service and prints its verdict and report link.
It needs Git history, and no account or cloud setup.

`putnami agent-readiness` measures how far a repository lets an agent work
safely. It reads twelve markers, grouped in four steps, and grades each one.
The
[agent-readiness method](/docs/platform/intelligence/agent-readiness-method)
says how. The method is neutral: it recognizes a tool by what the tool does,
and a Putnami command counts the same as any other tool.

This table says what each Putnami default gives each marker:

| Marker | What Putnami does by default |
| --- | --- |
| `understand.instructions` | `putnami init` writes a Putnami block in `AGENTS.md` that names the command to run and the gate to pass. |
| `understand.commands` | `putnami ci init` writes `putnami.ci.json`, which runs `lint`, `test` and `build` on every change, and `validate` when an extension of the workspace declares a `validate` job. |
| `understand.area-docs` | Every project template writes a `README.md`, and the `lint-docs` step of `putnami lint` fails on a broken relative link or anchor in a project's `README.md` files and `doc/` tree. |
| `bound.declared-areas` | Each project declares itself in its `putnami.json`, and `putnami.workspace.json` marks the workspace root. |
| `bound.boundary-rules` | A project is importable only from its own scope unless its `visibility` widens it, and an import that crosses a boundary fails every command that plans over the graph. |
| `bound.cross-area-changes` | `--impacted` runs what a change reaches; how often a change crosses areas depends on how you work, not on a default. |
| `verify.tests` | `putnami test` runs the tests of every selected project, and `--enforce-coverage` fails the gate below the coverage threshold. |
| `verify.static-checks` | `putnami lint` runs the linters of each language extension in the gate. |
| `verify.reliable-signal` | The Go and TypeScript lint refuses a skipped or focused test that no platform or dependency guard explains; a reviewed exception carries `putnami:allow-skip <reason>`. |
| `verify.pinned-toolchain` | `putnami.lock.json` pins each extension and template, and `putnami pin` adds the CLI to that lock. |
| `recover.ownership` | With `@putnami/sdd`, `validate-workspace` writes `.github/CODEOWNERS` from the `options.sdd.owners` each `putnami.json` declares, and CI fails when the committed file drifts. |
| `recover.small-changes` | An `--impacted` run reports the size of the change by category and warns on mixed intent; no line count fails it. See [Deliver in gated phases](#deliver-in-gated-phases-not-in-small-pull-requests). |

Two markers need a declaration from you: `recover.ownership` needs the owners,
and `verify.pinned-toolchain` needs a CLI pin. Some markers grade how long a
check has been enforced, so a new workspace reaches their top level only after
90 days of use.

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
- The MCP server runs **workspace jobs**. It does not deploy, does not reach
  external systems, and refuses jobs declared to do so.
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
