# 0004: A provider is declared under `providers` and speaks its own wire

- **Status**: accepted
- **Date**: 2026-10-29
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  `go.putnami.dev/protocol/extension` (`protocols/extension`, the `providers`
  manifest section), `go.putnami.dev/protocol/cli` (`protocols/cli`, contract
  18), the CLI route that calls providers, and the in-repository providers
- **Supersedes**: [ADR 0001](0001-collaboration-provider-contracts.md),
  decision 3 (a provider declares operations on its tools), the transport half
  of decision 5 (the route calls the operation through the extension tool
  transport, and the MCP server exposes the names) and the last sentence of
  decision 1 (the additive `ToolCallRequest.provider` member)
- **Amends**: [ADR 0003](0003-missions-contract.md), decision 2 and the
  consequence that the agent sees `missions.ask` and `missions.reply` as tools

## Context

The extension manifest had a `tools` section, built for the MCP server the core
used to serve. A tool carried a name, an input schema, four annotations and a
`_meta` object, and it ran as a subprocess that read one `ToolCallRequest` on
standard input and wrote one MCP result (`content[]` and `isError`) on standard
output.

[ADR 0082 of the CLI](../../../../tooling/cli/doc/adr/0082-putnami-serves-no-mcp-server.md)
deleted the server. After that change the `tools` section declared only
collaboration providers: github-collaboration (13 tools), local-collaboration
(12) and memory-store (4). Nothing read the tool name, the input schema or the
annotations as tool data. The wire kept the MCP shape for no reader:

- `Serve` dispatches on the `provider` member of the request and never reads the
  tool name.
- `ReadProviderOffer` checked that the annotations agreed with the catalog, so
  the declaration repeated what the catalog already says.
- The route wrapped a `collab.Response` in an MCP result and unwrapped it again.
- The `agent` member of the request had no producer.

Deleting `tools` without a replacement would break `putnami tasks`, `proposals`,
`memory` and `missions`, the host call `collaboration.call` and the contributor
hooks, which reach the missions provider through the host channel.

## Decision

1. **A manifest declares providers under `providers`, keyed by contract.** One
   entry per contract holds what every operation of it shares, and the
   operations:

   ```json
   "providers": {
     "tasks": {
       "version": 1,
       "command": "{extensionRuntime}", "args": ["provider"],
       "cwd": "{workspaceRoot}", "env": {}, "timeoutMs": 120000,
       "operations": {
         "find":    { "description": "…", "openWorld": true },
         "claim":   { "description": "…", "openWorld": true, "preconditions": "atomic" },
         "context": { "description": "…", "openWorld": true, "workspaceSelection": true }
       }
     }
   }
   ```

   An operation declares its `description`, whether it reaches outside the
   machine (`openWorld`), the rule it enforces for a revision
   (`preconditions`: `atomic`, `checked` or `none`) and whether it takes the
   resolved workspace view (`workspaceSelection`). The tool name, the input
   schema, the four annotations and `_meta` are gone: the catalog owns the
   access, the destructiveness and the request documents. `command`, `args`,
   `cwd`, `env` and `timeoutMs` keep their meaning, including the
   `{extensionRuntime}` placeholder. A provider is a subprocess: it gets no host
   RPC channel.
2. **`protocols/extension` owns the declaration and `protocols/collaboration`
   owns the wire.** `ProviderDeclaration` and `ProviderOperation` live in
   `protocols/extension`, next to the rest of the manifest. `ProviderRequest`
   lives here: the request of the former tool wire without the tool name and the
   `agent` member. It keeps the `provider` member that `Serve` dispatches on.
   `ProjectRef` and `Selection` move with it, with their drift test against
   `protocols/job`. The provider writes one `collab.Response` on standard
   output. The MCP result wrapper is gone.
3. **A manifest with `providers` requires CLI contract 19.** The new rung is
   `ProviderContract`, and `LatestContract` moves to 19. An older CLI that does
   not know the section refuses the manifest with the usual "requires a newer
   putnami" error instead of loading it with no provider. The extension
   `ProtocolVersion` stays 3: it versions the task contract, and nothing in the
   load ladder keys on it.
4. **There is no `tools` fallback and no overlap.** `ToolDefinition`,
   `ToolCallRequest` and `ToolCallResult` are deleted. A manifest that still
   carries `tools` loads leniently, as any unknown member does, and offers no
   provider; strict validation refuses it. Every call then answers
   `binding.incomplete` ("the extension does not implement the contract")
   without starting a process. This change carries a `BREAKING CHANGE:` footer
   that names the four extensions to update: `@putnami/github-collaboration`,
   `@putnami/local-collaboration`, `@putnami/memory-store` and
   `@putnami/intelligence`.
5. **The agent reaches `missions.ask` and `missions.reply` through
   `putnami agent ask` and `putnami agent reply`.** Both commands belong to
   `@putnami/contributor`. They call the operations through the host channel, as
   `sync` does, and record the asked decision and the reply for the current
   session. The provider wire does not change: `ask` and `reply` still take no
   session (ADR 0003, decision 1), and the next `sync` still binds what the
   session asked and answered. The `PostToolUse` hook no longer matches an MCP
   tool.
6. **The other members of the tool contract leave with it.** This change carries
   out ADR 0082, decision 9: `mcpResult` and the `MCPTool*` names in
   `protocols/cli`, the session `agent` member, `hostHooks[].mcpTools`,
   `features.surface.tools` (and with it D-022's mention of MCP tools),
   `tooling/extension-sdk/mcp`, the three `mcptool.go` bridges, and the
   `internal/dependencydocs` packages. The `cache verify` report origin `mcp`
   becomes `ephemeral`. Nothing is ported: the owner declined the dependency
   documentation tools and `sdd.architecture_context`.

## Rejected alternatives

- **Declare providers as `commands`.** An interactive command opens a host RPC
  channel, and a provider must get none. The command pipeline also adds flags,
  sessions and prerequisites that the provider wire does not need.
- **A flat map keyed `<contract>.<operation>`.** It repeats `command`, `args`,
  `cwd` and `timeoutMs` for every operation: 13 times in github-collaboration.
- **Read `tools` as a fallback.** It keeps the MCP wire and its annotation
  checks alive for one release, for providers that ship in the same release
  train.
- **Raise `ProtocolVersion` to 4.** The section changes the CLI's load ladder,
  not the task contract that the version pins.
- **Keep the MCP result wrapper around the response.** Putnami serves no MCP
  server (ADR 0082), so the wrapper has no reader.

## Consequences

- A provider extension rebuilt for this change loads only on a CLI that reads
  contract 19. Until `@putnami/github-collaboration`,
  `@putnami/local-collaboration`, `@putnami/memory-store` and
  `@putnami/intelligence` publish builds that declare `providers`, a workspace
  that pins an older build loses that binding.
- A provider author declares less: no schema, no annotations, no tool name. The
  package-time check is still the provider's own manifest test, which calls
  `ReadProviderOffer` on `providers`.
- `ReadProviderOffer` no longer compares annotations with the catalog. It checks
  that each required operation is declared once, the precondition rule, and
  `workspaceSelection` where the catalog says the operation takes the selection.
- A provider written in another language writes one JSON `Response` document on
  standard output and nothing else.
- The rest of ADR 0001 stays in force. ADR 0082, decision 9, is carried out by
  this ADR.
