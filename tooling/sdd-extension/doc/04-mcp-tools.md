# The MCP tools

`@putnami/sdd` contributes five read-only Model Context Protocol tools to
`putnami mcp`. Four answer the feature/spec questions extracted from core. The
fifth gives an agent one exact architecture domain from the same ARC/DARC
evaluator used by the interactive commands — never from a second parser or
graph.

| Tool | Answers | Arguments |
|---|---|---|
| `sdd.list_features` | The compact authored-feature catalog | `query`, `projects`, `impacted`, `baseline` |
| `sdd.feature_context` | One feature's scoped design facts | `feature` (required) |
| `sdd.list_specs` | The durable spec catalog | `projects`, `impacted`, `baseline` |
| `sdd.spec_context` | One complete spec document | `feature` (required) |
| `sdd.architecture_context` | One domain's complete declaration, touching relationships, findings, coverage, and diagnostics | `domain` (required, exact id) |

All five are annotated read-only and carry the `putnami.dev/contract` meta
(`access: "read"`, `readOnly: true`, `supportsDryRun: false`). None of them
writes. Creating a spec stays a `putnami specs init` decision made in a terminal
([ADR 0007](../../cli/doc/adr/0007-read-mostly-spec-surface.md)).

## The rename is breaking, and there is no alias

These tools were previously core tools named `list_features`,
`feature_context`, `list_specs`, and `spec_context`. **Those names are gone.**
The core server refuses them with JSON-RPC error `-32602`.

The dot is not a style choice. An extension tool name without a dot is dropped
by the CLI's extension-tool validator, and a name core owns wins the collision
against an extension — so an alias would have been either invisible or
ambiguous, depending on which of the two rules hit it first.

**What you have to update:**

- Agent configuration and prompts that name the old tools.
- Nothing in the workspace: the tool names are advertised live by the MCP
  server at initialization, not copied into a generated file.
- Any tooling that asserts on `tools/list` output.

## Calling them

```bash
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | putnami mcp

printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sdd.list_specs","arguments":{"projects":["@putnami/cli"]}}}' | putnami mcp

printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sdd.architecture_context","arguments":{"domain":"billing"}}}' | putnami mcp
```

`putnami mcp` prepares the extension runtime on the **first call**, not at
startup: registration answers the executable-presence probe from the manifest
declaration, so no extension code runs while the session is being set up.

## How a tool learns what the workspace contains

An MCP tool call carries no job context. Before these tools carried workspace
membership, a `ToolCallRequest` carried a `workspaceRoot` and nothing else — no membership, no
selection. A root-only handler is not merely limited, it is confidently wrong:
measured over this repository's parity fixture, `sdd.list_features` returned

```json
{ "selection": { "mode": "all", "scoped": false, "projects": [] },
  "manifests": 0, "features": [] }
```

with exit zero, over a workspace with two authored features.

The extension may not re-derive any of it — no directory scan, no `putnami`
subprocess — so the request contract grew instead, additively:

| The tool needs | The request member that carries it |
|---|---|
| Every workspace project, with its resolved version, type and direct edges | `workspaceProjects` |
| A **sibling**'s authored `putnami.json` (`bin`, `featureAuthority`, `options`) | `workspaceProjects[].config`, raw |
| What the caller asked for and what it resolved to | `selection`, byte-identical to the job wire's |

A tool opts in by declaring `"workspaceSelection": true` in its manifest
descriptor. The declaration is opt-in rather than implicit because `projects`,
`impacted`, and `baseline` are the CLI's vocabulary and not every extension's: a
third-party tool that means something else by `projects` must not have its
arguments read as project selectors.

`workspaceProjects[].config` is the member that is closed for **tools** and
still open for **jobs**. A tool reports on other projects by construction, so a
sibling's authored facts are part of its answer — `sdd.feature_context` reads a
project's `bin` to mint its command nodes, and without the member it would
report a project with no commands and say so confidently. A job acts on its own
project, so the same member is a follow-up there rather than a blocker.

**A request with no resolved `selection` is refused**, never defaulted to "all".
A wrong answer an agent cannot tell from a right one is worse than an error.

## Architecture context is worktree-only

`sdd.architecture_context` requires the exact `domain` minted by a
`putnami.architecture.json`; it does not guess from a path, owner, or final id
segment. Its typed report contains:

- the complete `DomainView`, including owned concepts, exports and imports;
- declared inbound edges (facts flowing into the domain) and outbound edges
  (facts it offers to consumers);
- every observed edge touching the domain;
- relevant ARC/DARC findings, detector coverage, ratchet summary, and sorted
  structural diagnostics.

The handler calls `BuildArchitectureWorktreeInspectionResult`, which calls the
same `EvaluateWorkspace` and bounded inspection projection as interactive
`architecture inspect`. The difference is deliberate: the agent tool never
resolves Git history or an adoption baseline. `baseline.compared` therefore
stays false. Interactive validate/snapshot/inspect remain baseline-aware and
unchanged.

Failures close at the boundary that discovered them. Missing or unknown
arguments and unresolved workspace selection fail before evaluation and carry
only an error. A structurally invalid repository, a blocking architecture
finding, or an unknown exact domain fails after evaluation and carries the
typed report first, then the error. An agent can therefore distinguish “the
tool did not run” from “the evaluator ran and rejected this worktree.”

## Parity with the tools they replace

`tooling/cli/internal/cli/sdd_mcp_parity_test.go` drives one real `mcp.Server`
built by production's own constructor and compares content blocks byte for byte
across seventeen calls for the four extracted tools — every narrowing their
schemas declare and every failure mode. Since the core tools were removed,
those answers are compared against **recorded fixtures** captured from core
before the deletion, with no `-update` flag; see [05-parity.md](05-parity.md).
Architecture context was added after that oracle was deleted, so its tests pin
the shared evaluator, full typed projection, worktree-only boundary, and
report-plus-error failure envelope directly.

Two implementation details are load-bearing and are the opposite of the
interactive path's:

- **No key-order round trip.** The interactive path re-encodes its payload
  through `map[string]any`, because the CLI captures a built-in command's
  stdout and Go sorts map keys. `handleToolsCall` does the opposite — it hands
  the handler's return value straight to `json.MarshalIndent` — so a report
  struct encodes in declaration order and applying the interactive adaptation
  here would *break* parity. Same extraction, opposite rule, one wire apart.
- **A failure may carry its report.** A core tool that fails with a value
  attached writes two content blocks, the value then the message. The SDK's
  `mcp.Serve` reproduces that split; argument guards return a nil payload, and
  engine calls return the report beside the error.
