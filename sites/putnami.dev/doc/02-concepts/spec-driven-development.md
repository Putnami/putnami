# Spec-driven development

Code review cannot hold the line alone anymore. Agents write a growing share of
every codebase, they write it fast, and they are eager to please: an agent will
happily "fix" a failing test by weakening it, satisfy a review comment by
deleting the behavior it questioned, or couple two modules that were never
supposed to know each other. Prompts and conventions do not stop this, because
a prompt is advice and advice is optional.

Putnami's answer is to record intent as strict data, next to the code, and to
make the CI gate — not the reader — check that code and intent still agree.
This page is the narrative: what gets declared, what gets proven, and where the
teeth are. The reference lives in the
[spec-driven development section](/docs/spec-driven-development).

## The loop

Five steps, each owned by a different artifact:

1. **Declare.** `putnami.features.json` mints a feature's identity: what it is
   called, who owns it, and its requirements — one durable ID each. Declaration
   is authority; nothing else may invent a feature.
2. **Specify.** One spec per feature (`specs/*.json`) records the prose no tool
   can derive: outcomes, non-goals, and links to the decision records the
   feature rests on. Specs are strict JSON — a malformed spec, a duplicate, or
   a reference to an undeclared feature fails the build.
3. **Prove.** Tests bind themselves to requirements in their runtime's native
   way — `spectest.Proves` in Go, `specTest` in TypeScript, the
   `putnami_proves` marker in Python. Every test run records which requirements
   were actually verified. Evidence comes from execution, never from assertion.
4. **Gate.** `putnami validate` joins the declared requirements with the
   recorded evidence. A project that sets its verification mode to `enforce`
   fails its gate on any unproven requirement. The default mode, `report`,
   observes without changing the exit code — adoption is a choice, per project.
5. **Ratchet.** Once a project enforces, its floor is committed to the
   `specs.baseline.json` in its directory. The floor only moves in one direction: the workspace
   validation — planned automatically whenever you run `validate` — fails any
   change that drops a project back to `report`
   or silently loses requirement coverage. Raising the floor is routine;
   lowering it requires editing that project's baseline in the same, reviewed
   change.

The division of labor matters: humans and agents author declarations, tests
produce evidence, and only the gate renders a verdict. An agent can read
everything and propose anything, but it cannot mark its own work as proven.

## Architecture contracts: ARC and DARC

Specs bound what a project *does*. Architecture contracts bound what domains
may *touch*.

**ARC — Architecture Rules as Code.** Each domain owns one
`putnami.architecture.json`: the projects it contains, what it exports, and
what it imports. From those manifests the validator builds the workspace's
architecture graph and compares it with reality: every observed cross-domain
project dependency must be covered by an exact, declared binding — this
consumer project, this producer project, nothing broader. An undeclared
dependency fails. A declared binding the workspace no longer observes also
fails, so the contract cannot rot into fiction.

**DARC — Domain Access & Replication Contracts.** The import half of a
manifest. A consumer declares *how* it accesses another domain's data: a
stable **reference**, a **query** to the owner, an immutable **snapshot**, a
rebuildable **projection**, or a **command**. A projection must spell out its
freshness bound, ordering, failure behavior, single writer, and rebuild story
before it is valid. The central rule behind all five modes:

> Data may be copied. Authority may not be copied.

Known debt is handled the same way spec coverage is: a baseline records the
violations that existed at adoption, it can only shrink, and debt never becomes
permission — a baselined edge warns, it does not turn into a declared right.

The Putnami workspace itself maps 83 of its 117 projects to ten domains —
`protocols`, `go-framework`, `typescript-framework`, `extension-providers`,
`cli`, `sdd`, `extension-sdk`, `agent-workflows`, `observability` and
`public-docs` — and validates their 417 cross-domain project dependencies on
every push, at zero findings and with no baseline. The other 34 projects are
samples and templates, each excluded with a reason and an owner: a sample proves
the public API and a template is a scaffold input, so neither owns a fact.

Most of those 417 edges are ARC permissions and nothing more. Six imports go
further and are enforced at run time: the documentation the site publishes for
five other domains, plus the runtime API it is built on. The protocol is
experimental: its wire format may still change without a migration path.

### Authored in code, enforced at run time

A manifest is JSON, and hand-writing several hundred lines of it does not scale.
Three surfaces close that gap, each stopping exactly where a human decision
starts:

- **Scaffold and reconcile.** `putnami architecture init <domain>` writes the
  first canonical manifest for a domain, carrying only the id, owner and
  projects you named. `putnami architecture sync` recomputes what follows from
  the resolved graph — dropping a project the workspace no longer contains, and
  a binding the graph no longer observes — and proposes a binding only for an
  edge whose contract is already declared. It never writes an **import**: an
  observed dependency with no declared contract is refused and named, and stays
  a failing finding until somebody declares it.
- **Authored in Go.** `go.putnami.dev/sdk/extension/architecture` builds a
  manifest through a fluent builder that runs the gate's own verdict at
  authoring time, and `Pin` holds a committed file to that program — the builder
  is the author, the JSON is its projection, and a hand edit fails the owning
  project's test run. Every domain in this repository is authored that way.
- **Enforced at run time.** `go.putnami.dev/app/darc` takes a declared import as
  *configuration*: a projection applies the freshness bound and the
  missing/stale behavior its manifest promises, hands the write handle only to
  the single writer the local model names, and rebuilds through the declared
  strategy. The manifest stops being a description of the code and becomes its
  configuration.

A registered component contributes one row to its project's capability manifest,
and `architecture validate` joins those rows to the declarations: an
implementation nobody declared fails, and — inside a domain that already
implements something — a declaration nothing implements fails too. Coverage says
exactly what happened: `framework-evidence`, never "observed", because a build
recorded what a component was configured with and nothing watched a request.

That makes runtime adoption **atomic per domain**. The first row a domain emits
opts it in, and from then on every one of its active imports is expected to be
implemented too. In this workspace two domains have opted in — `observability`
with two rows and `public-docs` with six — and the other eight emit nothing and
pass. The silence is deliberate: a capability row is written by an application
describing itself, and a CLI binary is not an application. Writing a row by hand
to raise the number would make the join a tautology, which is the one thing the
join exists to prevent.

Both framework runtimes enforce these contracts: `go.putnami.dev/app/darc` for
Go and the `darc` module of `@putnami/application` for TypeScript. A second
implementation of a contract's meaning is normally the drift this system exists
to catch, so neither runtime is its own oracle — both execute one shared corpus
of contracts and ordered operations, and a behavior that differs between them
fails on one side instead of shipping as two runtimes that describe the same
manifest differently. The corpus is the specification; a runtime that cannot
meet it is the thing that is wrong.

The code-first authoring builder stays Go-only, because extensions are Go. It is
optional there too: every domain, in any language, authors its manifest with
`architecture init`/`sync` and is held to the same gate.

## Why this shape works on agents

An agent inside a Putnami workspace meets this system three times:

- **Before writing** — the read-only MCP tools (`sdd.list_features`,
  `sdd.feature_context`, `sdd.list_specs`, `sdd.spec_context`) answer "what is
  this for, what were the non-goals, what did the team agree to" from declared
  data instead of the agent's guess.
- **While writing** — creating a spec is deliberately a terminal command
  (`putnami specs init`), not an agent tool. Declaring new intent is a human
  decision; proving it is machine work.
- **Before merging** — the gate
  (`putnami lint,test,build,validate --impacted`) replays
  everything above. The agent's opinion of its own work carries no weight; the
  recorded evidence does.

Every rule in this system shares one property: it is data an agent can read,
checked by a job an agent cannot argue with.

## Where to go next

- [Write a feature spec](/docs/how-to/write-a-feature-spec) — the authoring
  walkthrough.
- [Spec-driven development reference](/docs/spec-driven-development) — jobs,
  commands, MCP tools.
- [Agents](/docs/agents) — the full agent surface: context, blast radius, MCP
  contract.
- [Protocols](/docs/protocols) — the wire formats all of this rests on.
