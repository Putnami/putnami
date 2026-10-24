# MCP Server (`putnami mcp`)

Status: **graduated pilot surface**.
This chapter records the design, the SDK decision, and the evidence for
exposing the workspace through a Model Context Protocol (MCP) server instead
of making agents shell out to the `putnami` CLI for every operation.

## The question

The CLI already has strong agent affordances: `--output=jsonl` structured
events, `file:line:col` diagnostics, and an exit-code taxonomy
(`internal/cmderr`). So the bar for an MCP server is **measurable
improvement, not novelty**. An MCP server is an adapter over the same
internals (`internal/workspace`, `internal/jobs/planner`,
`internal/jobs/scheduler`, `internal/output`) speaking
JSON-RPC 2.0 over stdio — spawned by the client, no hosting, no auth, dies
with the session.

## Fresh worktrees

MCP does not require a preceding `putnami install`. Exact locked extension
descriptors retain their five-second startup preparation budget. The first tool
request for graph facts prepares a missing provider index, with a
separate deadline of 60 seconds; initialization itself does not wait for probes.
This graph preparation uses the shared workspace probes to create the disposable
`.putnami/workspace-index.json`. It does not run `workspace-install` or
`workspace-sync`, install project dependencies, configure Cloud, regenerate
agent files, or update the committed lock.

Concurrent CLI and MCP preparation coordinates index publication and rechecks
freshness before doing work. The server reloads an index created or replaced
after startup. If preparation cannot complete, graph tools retain their explicit
unavailable result and repair instruction; a later request can retry. A usable
recorded index retains the existing freshness reporting. A `run_jobs` dry run
keeps its preview-only behavior and does not persist this preparation.

See [ADR 0024](adr/0024-lazy-workspace-initialization.md) for the preparation,
concurrency and output boundaries.

## What was built

`putnami mcp` starts a single-session JSON-RPC 2.0 server on stdin/stdout
(the MCP "stdio" transport: one JSON object per newline-delimited line). It
implements the MCP lifecycle (`initialize`, `notifications/initialized`,
`ping`), tools (`tools/list`, `tools/call`), and workspace resources
(`resources/list`, `resources/read`).

`initialize` answers with the protocol version the client requests when the
server speaks it: `2024-11-05`, `2025-03-26`, `2025-06-18` or `2025-11-25`. It
answers any other requested version, and a missing one, with `2025-11-25`, and
never echoes a version it does not speak. The `./putnamiw mcp` relay that
answers `initialize` while the CLI builds follows the same rule. Both answer
`server/discover` with "method not found", which is how Claude Code falls back
to `initialize`.

One inbound frame is capped at **4 MiB** including its terminating newline
(`maxFrameBytes` in `internal/mcp/server.go`). The framing carries no length
prefix, so the cap is enforced while the frame is being read: a client that
streams a line without ever terminating it cannot make the server allocate past
the cap. An oversized frame is answered with a single `-32600` error and a
`null` id — the frame was never parsed, so there is no request id to echo — and
the stream is resynchronized at the next newline, so the session survives one
bad frame exactly like it survives an unparseable one. The cap is far above any
legitimate request (the largest is a `run_jobs` call enumerating every project,
tens of KiB) and deliberately tighter than the 16 MiB allowed for
machine-generated JSONL job events, which carry whole asset manifests.

The **core** tool surface is eleven tools (it was fifteen before moving
the four SDD tools into `@putnami/sdd` — see *Extension-contributed tools*
below):

| Tool | Purpose | Adapts |
|------|---------|--------|
| `list_projects` | All projects with id/name/path/type/tags/deps | `workspace.Load` |
| `describe_project` | One project's deps, dependents, extensions, publish | `workspace` + graph |
| `agent_context` | One project's deterministic orientation document plus native feature summaries and artifact status | context-pack aggregator + design graph |
| `workspace_map` | The whole workspace orientation map — project paths, dependency and intercall edges, endpoints, config keys, schemas, docs — rebuilt in memory, scopable by `section`/`project` | `mapgen` reduce over validated fragments |
| `deps` | Direct or transitive dependencies/dependents for one project | `DependencyGraph` |
| `find_owner` | File path → nearest owning project, plus asset claimants | `workspace.ProjectOwnersForPath` |
| `why_impacted` | Shortest path explaining why A impacts B, over the same dependency, contract and extension-consumer edges `impacted` widens through, each hop with its kind and the target's task scope | `workspace.ImpactPath` |
| `topo_sort` | All projects in dependency-first topological order | `DependencyGraph.TopologicalOrder` |
| `impacted` | Projects affected by git changes vs a baseline, with a reason per project (the changed file that claimed it, or the edge that reached it) | `workspace.ImpactedSelectionForBaseline` |
| `run_jobs` | Run build/test/lint; return **summary + failures-with-diagnostics only**; `dryRun` returns the planned DAG without executing | `planner.Plan` + `scheduler.RunPlan` |
| `get_diagnostics` | Re-read the last run's diagnostics without re-running | cached run result |

Every tool advertises read-only/mutating metadata through MCP annotations and
a `putnami.dev/contract` `_meta` block. The only mutating tool is `run_jobs`,
which is marked as mutating `workspace`/`cache` and supports `dryRun`.

The resource surface is intentionally narrow. The server lists each resource by
a concrete URI, not a resource template, and renders it on every
`resources/read`. A resource whose builder is not injected is left out of the
list. Listing prepares no workspace graph.

| Resource | Purpose | Adapts |
|----------|---------|--------|
| `workspace://context` | Live workspace layout, graph, topological order, publish channels, warnings, and pinned lockfile versions | `workspace.Load` + `putnami.lock.json` |
| `putnami://extension-guidance/AI.md?name=…&version=…` | Exact `AI.md` bytes of one lock-pinned extension, or an explicit unavailable status | lock-pinned extension artifacts |
| `putnami://workspace/guidance` | The Putnami block body of `AGENTS.md`, decisions included | `agentctx.WorkspaceGuidanceBody` |
| `putnami://workspace/map`, `putnami://workspace/map/<section>` | The `workspace_map` answer for section `all`, or for `projects`, `dependencies`, `intercalls`, `apis`, `config-keys`, `schemas` or `docs` | the `workspace_map` builder |
| `putnami://projects/<id>/context` | One project's `agent_context` answer. `<id>` is the canonical project id escaped as one path segment, for example `%2Ftooling%2Fcli` | the `agent_context` builder |

The server notices a change of the agent context between two requests. No file
watcher runs. Before it answers a `tools/call` or a `resources/read`, it hashes
`putnami.workspace.json`, `putnami.lock.json`, `putnami.ci.json`, the manifest
and `AI.md` of each extension loaded by path, and the `decisions.json` files its
last workspace load found (`internal/mcp/context_change.go`). When the digest
differs from the one the session last heard about, the next tool result ends
with one more text block. It names each change and what to read:

```text
Putnami notice: the workspace context changed.
- D-025 added; putnami.ci.json changed. Read putnami://workspace/guidance
- @putnami/go 0.3.1 installed, replacing 0.3.0. Read putnami://extension-guidance/AI.md?name=%40putnami%2Fgo&version=0.3.1
- Putnami reloaded its extension tools and guides; list the tools again.
```

A change is reported once. A `resources/read` notices it but carries no notice,
because a resource has no free-text slot, so the next tool result reports it. A
tool result is the one place the notice reaches the model: neither Claude Code
nor Codex shows a notification to it. A line points at a resource only when the
server lists it and the resource shows the change. Otherwise it points at the
changed file: `putnami.lock.json` for an extension installed without a listed
guide, and `putnami.workspace.json` for a version constraint or an option. The
`decisions.json` of a project a workspace load finds for the first time, after
a load that failed or a change to the `includes` for example, joins the
baseline as it is, so only a later edit to it is reported.

When the extensions changed (an extension added, removed or disabled, a locked
version, or a path manifest), or the `includes` or `options.collaboration`
changed, the server loads the workspace again and reloads its extension
selection, guides and tools. It then writes `notifications/tools/list_changed`
and `notifications/resources/list_changed`, in that order, before the response
of the request that noticed the change. The server makes its start selection
the same way, after its first read of the inputs. The tools count as built
from the inputs as they were before the start or the reload, so an edit that
lands during either is loaded by the next request. A call to a tool the reload removed returns an
error tool result that says to list the tools again and carries the notice. A
name the server never registered stays an invalid-params error. A project's own
`putnami.json` is not an input: a change there reloads nothing. `initialize`
declares `listChanged: true` for tools and resources, and the server writes
nothing after its session ends.

`run_jobs` reuses the exact CLI building blocks (`extension.DiscoverExtensions`
→ project selection → `planner.Plan` → `scheduler.RunPlan`) with
a **no-op renderer**, so job output never touches stdout (the JSON-RPC channel)
and tool results stay consistent with the CLI. Diagnostics are extracted from
each job's result events, mirroring `output.RenderDiagnosticEvent`'s location
parsing.

### Files

```
internal/mcp/protocol.go   JSON-RPC 2.0 + MCP message types and constants
internal/mcp/server.go     stdio read/write loop, lifecycle, dispatch
internal/mcp/tools.go      tool registry, JSON schemas, tools/call handler
internal/mcp/resources.go  resource registry + workspace resource reads
internal/mcp/adapter.go    the Putnami logic behind each tool + no-op renderer
internal/commands/mcp.go   thin entry point bound to os.Stdin/os.Stdout
internal/mcp/testdata/contract.json  golden MCP contract snapshot
```

The implementation stays small and stdlib-only: a JSON-RPC loop, a tool
registry, a resource registry, and adapters over existing workspace/job
internals.

## Extension-contributed tools

Extensions can contribute namespaced MCP tools through the `tools` map in
`putnami.extension.json`. `putnami mcp` discovers descriptors alongside normal
extension manifests and merges them into `tools/list` after the core tools. It
launches the extension-owned command only for the corresponding `tools/call`,
writing one request JSON value to stdin and translating its one result JSON
value back to the MCP client.

The boundary is deliberately narrow: tool descriptors, one call, one result.
It does not expose a general extension RPC channel or import extension code
into the CLI. Authenticated network traffic remains outbound and
extension-owned—the same trust boundary as existing extension commands—while
the core server remains free of credentials and runtime dependencies.

`@putnami/sdd` is the first-party proof that the boundary is wide enough for a
real surface: its four tools (`sdd.list_features`, `sdd.feature_context`,
`sdd.list_specs`, `sdd.spec_context`) were **core** tools under undotted names
before they moved out through this path with no core code left behind
and no alias for the old names, which are now refused with `-32602`.

Extension tool names are required to be namespaced (for example,
`putnami.search`). Core names win, and duplicate names across extensions are
rejected rather than selected by discovery order. That pair of rules is why the
SDD rename could not be softened with aliases: an undotted extension name is
dropped by the validator, and a name core owns loses the collision. Every tool declares MCP
safety hints plus `putnami.dev/contract` metadata. Discovery is manifest-only
and process startup is lazy; malformed descriptors, duplicate names, or a
missing executable are excluded so the session degrades to the remaining core
and healthy extension tools.

A tool may name `{extensionRuntime}` as its command, like every task does. That
token is not in `BuildTemplateVars` — the orchestrator supplies it only where it
owns a prepared runtime — so this path resolves it from the extension's own
description, and prepares the runtime on the FIRST CALL rather than at startup.
The timing is the contract: registration still runs no extension code, and a
runtime that will not build fails the call it belongs to instead of the session.
Registration answers the executable-present probe from the DECLARATION for such
a tool, since there is nothing to stat before the first call.

A tool that declares `"workspaceSelection": true` also receives the workspace
view on its request: `workspaceProjects` (the complete resolved membership, with
each project's resolved version, type, direct edges and authored `putnami.json`)
and `selection` (the projection its `projects` / `impacted` / `baseline`
arguments resolved to, through the CLI's canonical resolver). Both members are
absent for a tool that does not declare it. The declaration exists so an
extension never has to re-derive workspace facts — a second impact algorithm or
a second project-identity resolver would diverge from this one — and it is
opt-in because those three argument names are the CLI's vocabulary, not every
extension's.

## Decision: hand-rolled vs official Go SDK

**Recommendation: hand-rolled, stdlib-only.**

The CLI's stated design philosophy (chapter 01) is *zero external runtime
dependencies* — every package uses only the Go standard library, which
"eliminates supply chain risk and simplifies builds: `go build` produces a
single static binary." Adopting `github.com/modelcontextprotocol/go-sdk` would
be the **first** third-party runtime dependency in `tooling/cli`, contradicting
that principle for a protocol that is, for our purposes, small and stable:

- The stdio transport is newline-delimited JSON-RPC 2.0 — a `bufio.Reader`
  loop and `encoding/json`. No framing library needed.
- We use a narrow slice of MCP: lifecycle, tools, and read-only resources.
  We do not need prompts, sampling, roots, or streaming; the only
  notifications out are the two list-changed ones.
- The hand-rolled surface remains fully unit-testable without a live client
  (see `server_test.go` and `contract_test.go`).

The cost we own is spec upkeep: if a future MCP revision changes the lifecycle
or tool-result/resource shape, we update `protocol.go`. Adding
`workspace://context` deliberately re-opened the SDK decision from the spike;
the current `resources/list` + `resources/read` implementation remains small
and read-only, so the stdlib path still wins. Revisit the SDK if the surface
widens into multiple resources, prompts, streaming, roots, or server-initiated
notifications.

## Evidence per hypothesis

The prototype was exercised end-to-end against the live workspace (79
projects). Two caveats on rigor: a full agent A/B (token + wall-time across
3–4 tasks, run twice) needs a live agent harness and is the **remaining manual
step** this spike scaffolds rather than completes; and N is small. What follows
is what is concretely measurable from the prototype itself, plus a reasoned
verdict.

### H2 — Context cost (measured) ✅ strongest signal

Same operation (`lint,test,build` on `/tooling/cli`, fully cached), raw CLI
event stream vs curated MCP tool result:

| | Lines | Bytes |
|---|------|-------|
| `putnami … --output=jsonl` (raw stream an agent would parse) | 42 | 8762 |
| `run_jobs` curated result (summary + failures only) | 14 | 176 |

**~50× fewer bytes** for one 8-job project. The raw stream emits
`task:start`/`task:event`/`task:end` per task plus a `session:end`, so it grows
linearly with plan size; the curated summary stays flat and only expands with
*failures*. (The measurement above predates the machine-contract flip
and was taken on the v1 `job:*` envelopes. The v2 records carry a typed
`identity` per line, so the raw side is now *larger* and the ratio only widens —
the conclusion is unaffected, which is why the numbers are left as recorded.) On a large `--impacted` plan (hundreds of event lines) the gap
widens. `get_diagnostics` then lets the agent pull failure detail on demand
instead of pre-paying for it. This is the most defensible win.

### H1 — Discoverability ⚖️ plausible, not yet measured

`tools/list` returns typed JSON-Schema inputs (required fields, enums,
descriptions) the harness can read directly, versus reading generated context +
`putnami --help` and guessing flags. Whether this reduces wrong first calls is
exactly the kind of thing the agent A/B would quantify. Qualitatively the
schemas are precise (e.g. `run_jobs.commands` is `array<string>`, `impacted`
documents the `baseline` default), which should shrink the guess space.

### H3 — Harness integration ⚖️ plausible, environment-dependent

Dedicated tools (`run_jobs`, `impacted`) can be allowlisted/parallelized by an
agent harness, whereas `bash putnami …` is one opaque permission surface. The
size of this win depends on the harness's permission model; the prototype makes
the tools available but does not by itself prove a reduction in prompts.

### H4 — Error round-trips ⚖️ partially realized

Tool errors are returned as structured `isError` results with a specific
message (e.g. `project not found: X`, `commands is required`), and `run_jobs`
failures carry per-job `error` + `diagnostics[]` with `file:line:col` — no
stderr scraping. This is a cleaner contract than exit-code + stderr text;
whether it cuts retry loops is, again, for the A/B to confirm.

## `.mcp.json` (distribution)

Distribution is free — the server ships in the existing binary. Claude Code
discovers it through a workspace-root `.mcp.json`:

```json
{
  "mcpServers": {
    "putnami": {
      "command": "putnami",
      "args": ["mcp"]
    }
  }
}
```

`putnami init`, `putnami install` and `putnami upgrade` add a missing
`putnami` entry to this file, and `putnami mcp install` is the explicit form
([ADR 0040](adr/0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md)).
The
writer is merge-aware and non-destructive: other servers and unknown keys are
preserved, and an unparseable file is left untouched. A diverged `putnami`
entry is kept by the implicit path and rewritten only by `putnami mcp install`,
which is what repairs a hand-edited registration. The bare `putnami` command
name resolves from PATH per machine, so the committed file is portable across
the team. A source workspace refuses the global binary, so when its root holds
a `putnamiw` file the entry's command is `./putnamiw`. That choice reads only
committed content (the lock and the wrapper), never the file mode, so macOS,
Linux and Windows write the same entry for the same commit.

This repository commits its own root `.mcp.json`. It is a source workspace, so
the entry runs `./putnamiw mcp`, which builds the CLI from the tree.

### Optional agent request provenance

The server retains the bounded `initialize.clientInfo.name` and
`initialize.clientInfo.version` for the lifetime of its stdio session. MCP has
no standard active-model field, so Putnami does not infer one from an IDE,
Conductor, or provider default. A harness can explicitly provide it through
the MCP server environment while opting into propagation:

```json
{
  "mcpServers": {
    "putnami": {
      "command": "putnami",
      "args": ["mcp"],
      "env": {
        "PUTNAMI_AGENT_IDENTITY": "1",
        "PUTNAMI_AGENT_MODEL": "haiku"
      }
    }
  }
}
```

When enabled, Putnami-owned HTTP clients and cooperating extension clients can
send:

```http
User-Agent: putnami-mcp/<version> (harness=claude-cli/1.0; model=haiku)
Putnami-Agent-Harness: claude-cli/1.0
Putnami-Agent-Model: haiku
```

Extension tool subprocesses receive a structured `ToolCallRequest.agent`
object (the source of truth) and matching `PUTNAMI_AGENT_HARNESS`,
`PUTNAMI_AGENT_MODEL`, and `PUTNAMI_AGENT_USER_AGENT` environment variables.
If the opt-in or usable metadata is absent, requests keep the existing
`putnami-cli/<version>` User-Agent and no identity headers or subprocess fields
are added.

Client/model values are trimmed, converted to printable ASCII where needed,
limited to 128 characters, and rejected entirely if they contain ASCII control
characters. No prompts, credentials, token counts, user identifiers, or
session secrets are collected. Request headers are commonly recorded by API
servers and intermediaries; operators should enable this only where those logs
are an acceptable place for harness/model provenance.

## Contract stability

The agent-facing contract is pinned by
`internal/mcp/testdata/contract.json`. `internal/mcp` tests compare the advertised
tools and resources to that golden snapshot, so schema, metadata, and resource
drift fail in CI. Regenerate after an intentional contract change with:

```bash
cd tooling/cli
UPDATE_MCP_CONTRACT=1 go test ./internal/mcp -run TestContractMatchesToolingBaseline
```

## Go / no-go

**Go — graduate the MCP surface.**

The implementation is real, stdlib-only, tested, and the original H2 context
cost signal is now backed by a recorded A/B pilot for H1/H3/H4. On 2026-06-18,
six CLI runs and six MCP runs were recorded across three tasks
(`discover-workspace`, `plan-impacted-jobs`, `explain-impact-path`) using fresh
Claude CLI sessions, and a one-off analyzer turned them into this table. The
ledger and the analyzer are removed; the numbers below are the record.

| Variant | Runs | Success | Avg Tokens | Avg Wall Ms | Avg Tool Calls | Avg Retries |
|---------|------|---------|------------|-------------|----------------|-------------|
| CLI | 6 | 83% | 79168 | 25544 | 7.0 | 7.0 |
| MCP | 6 | 100% | 55813 | 21496 | 5.0 | 5.0 |

The analyzer recommendation is `go`: MCP success was at least CLI success and
improved tokens, tool calls, and retry loops in the recorded pilot.

The measured win is not a license to expand the surface casually. Future write
tools still need an explicit opt-in contract, and future resources should repeat
the same measurement discipline instead of assuming MCP is always the cheaper
interface.

### Graduated follow-up

- Tool/resource schemas are pinned by `internal/mcp/testdata/contract.json`.
- `.mcp.json` registration is written by `init`, `install` and `upgrade` when missing, repaired by `putnami mcp install`, and committed at this workspace root.
- Semantic graph tools (`deps`, `find_owner`, `why_impacted`, `topo_sort`) wrap existing
  graph/impact internals plus shortest-path and topological-order helpers.
- `workspace://context` provides live graph/layout/channel/version context from
  the workspace load path, without adding a second generator.
- Write tools beyond `run_jobs` remain out of scope unless added behind an
  explicit opt-in contract.

## Trying it

```bash
# From a workspace root:
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_projects"}}' \
  | putnami mcp
```
