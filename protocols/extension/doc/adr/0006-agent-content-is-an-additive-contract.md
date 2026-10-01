# ADR 0006 — Agent content is an extension contribution under an additive contract

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`agentContent`),
  `go.putnami.dev/protocol/cli` (`protocols/cli/contract.go`), and the SDK
  package step (`tooling/extension-sdk/agentartifact`)

## Context

Agent instructions (skills, worker profiles, references, helper scripts and
their host adapters) describe an extension's commands and MCP tools. Shipped
as separately resolved agent artifacts, the tool and its instructions carry
two versions that drift apart. The extension manifest is their natural home,
and three facts decide how it carries them:

- **The CLI loader decodes permissively.** `NegotiateManifest` uses
  `json.Unmarshal`, so a CLI that predates a member drops it without a word.
  For agent content the silent drop is the whole failure: the workspace runs
  the extension without the instructions it opted into.
- **The only member an older reader negotiates is `cliContract`.** Its ladder
  refuses a stamp above its own latest contract with "requires a newer
  putnami".
- **Raising the base contract breaks every published extension.** The ladder
  rejects a stamp below the required contract, and every runtime handshake
  reports `CurrentContract`, which the CLI compares for equality.

Extension content is published to consumers whose CLI is not built from this
repository, so the lock-pinned argument of
[ADR 0002](0002-runtime-toolchains-are-lock-pinned-under-the-current-contract.md)
decision 7 does not cover it.

## Decision

### 1. `agentContent` is an optional contribution

The built content is an agent-artifact tree (`putnami.agent-artifact.json`
plus the files it declares), materialized under the CLI's existing ownership
rules ([CLI ADR 0004](../../../../tooling/cli/doc/adr/0004-agent-artifact-ownership.md)).
The section has two forms:

- **authored**: `path` and `source`, the extension-relative directory of the
  closed authoring layout (`skills/`, `agents/`); a local extension is built
  from it on every run;
- **packaged**: `path` and `manifestSha256`. The package step builds `source`
  into `path`, drops `source`, and records the digest of the built manifest.

Paths are canonical, extension-relative, never the root, and never overlap. A
manifest must contribute a command, an MCP tool or agent content, so a
content-only extension is valid. An empty section is `empty-agent-content`.

### 2. `supersedes` names the agent artifacts the content replaces

Only the extension's author knows that its content replaces separately
declared artifacts (for example `@putnami/agent-workflows`); renamed skills
do not collide on paths yet still compete. `supersedes` lists agent-artifact
identities:

- each entry is a registry name (`@scope/name` or `name`), appears once, and
  is not the extension's own name, which would make one ownership record both
  source and target of a migration (`invalid-agent-content-supersedes`);
- it belongs to both forms; the package step copies it unchanged and emits no
  member when the list is empty;
- it names identities, not versions: the migration reads the superseded
  artifacts at the versions the consumer pinned and shows every changed byte.

The CLI refuses a workspace that opts into the content while a superseded
artifact is still declared or recorded, and `putnami migrate agent-content
<name>` reads the list
([CLI ADR 0048](../../../../tooling/cli/doc/adr/0048-migrating-agent-artifacts-to-extension-content.md)).
An extension without `supersedes` offers no migration, and the command says
so.

### 3. Contract 5 is additive; the stamp is the lowest that covers a manifest

`protocols/cli` defines `AgentContentContract = 5` and `LatestContract = 5`.
`CurrentContract` stays 4: the base stamp and the contract every runtime
handshake reports.

- `RequiredCLIContract(m)` is 5 for a manifest that declares `agentContent`
  and 4 otherwise. The package step writes it and the loader enforces it.
- The loader accepts every stamp from the required contract up to
  `LatestContract`. A content manifest stamped 4, or unstamped, is refused
  with the re-package remedy; agent content is a contract surface, never
  exempt as hook-only.
- A contract-4 CLI sees 5 above its latest contract and refuses the package.

Every other manifest keeps its stamp and loads in both directions, and no
handshake moves. The next additive member follows the same rule: a new
constant, a `RequiredCLIContract` branch, a changelog entry.

### 4. The digest chain binds content to the resolved extension

The lock pins the extension manifest digest, `manifestSha256` pins the content
manifest, and the content manifest pins every file. The content's identity and
version are the extension's; the CLI refuses a tree whose manifest names
anything else. Instructions and tools cannot drift apart.

### 5. The package step is the gate

`agentartifact.PackageExtension` packages a content-only extension: strict
parse and validation of the authored manifest, a build under the project's
content policy (`options.agent-artifact`), the packaged form and required
stamp written, strict validation of the result, and the loader as its
postcondition. It refuses an extension with a runtime, commands, tools,
tasks, hooks or a workspace adapter, and an authored stamp above the latest
contract. The archive is reproducible and platform independent. The Go and
npm packagers stage content through the same step
([extension-sdk ADR 0003](../../../../tooling/extension-sdk/doc/adr/0003-every-packager-ships-agent-content-through-one-step.md)).
The workspace opt-in and lifecycle are
[CLI ADR 0047](../../../../tooling/cli/doc/adr/0047-extension-owned-agent-content.md).

## Rejected alternatives

- **Raise `CurrentContract` to 5.** Every published extension stamped 4 stops
  loading and every runtime handshake stops matching.
- **Rely on the schema or strict parsing.** The permissive loader runs
  neither.
- **A sibling content document an old reader never opens.** The old CLI would
  install the extension and ignore the file: the silent drop.
- **Build the source at install time.** Consumer bytes would depend on the
  installing CLI's builder, breaking published digests.
- **Let the consumer name the superseded artifacts.** The mapping is a fact
  about the content; a wrong one moves ownership of files it never replaces.
- **Infer supersession from overlapping paths.** Overlap is already refused,
  and renamed skills compete without overlapping.
- **Version ranges per superseded artifact.** The migration is byte-based and
  shows every change, so a range restricts nothing.

## Consequences

- A contract-4 CLI refuses a content-bearing package. In a workspace that
  opted in it also fails `putnami install`: it reads the `extension:<name>`
  opt-in as an agent artifact named `extension` that no lock pins, and writes
  nothing. `TestAgentContentOptIn_OlderReaderFailsClosed` pins that reading.
