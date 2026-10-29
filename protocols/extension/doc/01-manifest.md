# Extension Manifest

Every Putnami extension is defined by a `putnami.extension.json` manifest file at the extension root. The manifest declares what commands the extension provides, how they're implemented, and what runtime dependencies are needed.

## Minimal Manifest

The smallest valid extension manifest has a single command with at least one pipeline step:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@acme/example",
  "commands": {
    "build": {
      "run": [{ "id": "build", "task": "build-exec" }]
    }
  },
  "tasks": {
    "build-exec": {
      "kind": "command",
      "command": "echo",
      "args": ["built"]
    }
  }
}
```

## Top-Level Fields

| Field | Required | Description |
|-------|----------|-------------|
| `$schema` | no | JSON Schema reference for editor support |
| `name` | yes | Extension identifier (e.g., `@putnami/typescript`): a scoped name (`@scope/name`) or a kebab-case identifier |
| `version` | no | Extension version (semver) |
| `cliContract` | no | CLI ↔ extension contract version, stamped by the package job after strict validation (earned, not claimed — see [validation](04-validation.md)) |
| `commands` | one of `commands`, `providers`, `agentContent`, `verbs` | Public commands exposed to users |
| `tasks` | no | Atomic executable units referenced by commands |
| `extensionDependencies` | no | Other extensions this extension needs, each with a version constraint. The CLI installs, pins and loads each registry dependency (`@scope/name`); a manifest that names one requires CLI contract 8 — see below |
| `workspaceDevDependencies` | no | NPM devDependencies required at workspace root |
| `autoServe` | no | Whether `serve` auto-discovers projects with this extension |
| `runtime` | no | The extension's own executable and how to prepare it — see [lifecycles](07-lifecycles.md) |
| `workspace` | no | Workspace adapter: markers, metadata inputs, exclusions, sync task — see [lifecycles](07-lifecycles.md) |
| `hooks` | no | Lifecycle hooks (e.g., `preBuild`, `onInstall`) |
| `contracts` | no | Input/output schemas for task validation |
| `optionNamespaces` | no | Bare `options.<name>` blocks of a project config this extension reads — see below |
| `providers` | one of `commands`, `providers`, `agentContent`, `verbs` | The collaboration providers the extension implements, keyed by contract. Requires CLI contract 18 — see below |
| `agentContent` | one of `commands`, `providers`, `agentContent`, `verbs` | Agent instructions the extension ships under its own version — see below |
| `verbs` | one of `commands`, `providers`, `agentContent`, `verbs` | The verb vocabulary: verbs other extensions run, with their descriptions, aliases, default traits and related commands. Requires CLI contract 9 — see [verbs](06-verbs.md#declaring-a-verb-vocabulary) |

A manifest must name its extension. Strict validation, `putnami extensions
validate` and the packagers refuse one without a `name`, with a `required-field`
diagnostic on `name`; a hook-only manifest the packagers publish needs a name
too. The loader does not apply the rule: an installed artifact released without
a name keeps loading under the reference that installed it. Discovery takes the
extension name from the manifest, else from the `name` of the `putnami.json` in
the extension directory, else from the reference, and the version from the
manifest alone. It reads no `package.json`.

A manifest must contribute something: at least one command, provider,
agent-content contribution, or verb vocabulary. A content-only extension — no
runtime, no command, no provider — is a complete manifest, and so is a
vocabulary-only one.

## Providers

`providers` declares the collaboration contracts the extension implements
(`tasks`, `proposals`, `memory`, `missions`), keyed by contract. One entry holds
what every operation of the contract shares, and the operations:

```json
{
  "name": "@acme/tracker",
  "providers": {
    "tasks": {
      "version": 1,
      "command": "{extensionRuntime}", "args": ["provider"],
      "cwd": "{workspaceRoot}", "timeoutMs": 120000,
      "operations": {
        "find":    { "description": "Find issues in the tracker.", "openWorld": true },
        "claim":   { "description": "Assign the issue atomically.", "openWorld": true, "preconditions": "atomic" },
        "context": { "description": "Load the issues of the selected projects.", "openWorld": true, "workspaceSelection": true }
      }
    }
  }
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `version` | yes | The contract version the provider implements, 1 or above |
| `command`, `args`, `cwd`, `env`, `timeoutMs` | `command` | The subprocess every operation runs, with the same placeholders as a task, `{extensionRuntime}` included. `timeoutMs` defaults to 30 seconds |
| `operations.<name>.description` | yes | The provider-specific behavior and limits of the operation |
| `operations.<name>.openWorld` | yes | Whether the operation reaches a system outside this machine |
| `operations.<name>.preconditions` | no | How the provider enforces a revision precondition: `atomic`, `checked` or `none` |
| `operations.<name>.workspaceSelection` | no | Ask the CLI to resolve the workspace view (the complete membership and the resolved `projects`, `impacted` and `baseline` selection) and put it on the request |

The CLI writes one provider request on the command's standard input and reads
one collaboration response from its standard output. A provider gets no host
RPC channel. Strict validation checks the shape. The contract names, the
operations, which operations a version requires, which take a precondition or
the workspace view, and both documents belong to
[`protocols/collaboration`](../../collaboration/README.md), whose
`ReadProviderOffer` checks a declaration against the catalog.

Declaring `providers` requires `cliContract` 18 (`ProviderContract`), wherever
the manifest loads from: a CLI below 18 refuses the manifest instead of loading
the extension with no provider. There is no `tools` section: a manifest that
still carries one loads leniently with no provider, and strict validation
refuses the member.

## Agent Content

`agentContent` declares the agent instructions an extension ships: skills,
worker profiles, references and helper scripts, projected onto every supported
agent host (`.agents/skills/**`, `.claude/skills/**`, `.claude/agents/**`,
`.codex/agents/**`). The built content is an agent-artifact tree — a
`putnami.agent-artifact.json` manifest plus every file it declares, the same
format a standalone agent artifact uses — so the CLI materializes it under the
same ownership rules.

The section has two forms, and a manifest carries exactly one:

| Form | Fields | Who writes it |
|------|--------|---------------|
| Authored | `path`, `source` | The extension author. `source` is the extension-relative directory of the closed authoring layout: `skills/<name>/SKILL.md` with optional `agents/{claude,openai}.yaml`, `references/**` and `scripts/**`, and `agents/<name>/{AGENT.md,claude.yaml,codex.toml}`. |
| Packaged | `path`, `manifestSha256` | The package step. It builds `source` into `path`, drops `source`, and records the SHA-256 of the built agent-artifact manifest. |

```json
{
  "name": "@acme/contributor",
  "agentContent": { "path": "agent-content", "source": "agent-src" }
}
```

Both directories are canonical and extension-relative: no escape, no absolute
path, not the extension root, and neither may contain the other. The content
policy — forbidden content and required skills — is the extension project's
`options.agent-artifact` in its `putnami.json`, exactly as for an agent-artifact
project, and the build refuses content that breaks it.

A packaged contribution is bound end to end: the workspace lock pins the
extension's manifest digest, the manifest pins the content manifest digest, and
the content manifest pins every file digest. The content's identity and version
are the extension's, so an extension that also ships commands or providers ships
its matching instructions under the same resolved version.

Declaring `agentContent` requires `cliContract` 5, the additive contract the
package step stamps on exactly these manifests. A CLI that predates the section
refuses the package ("requires a newer putnami") instead of loading the
extension without its instructions. See [validation](04-validation.md).

Installing an extension never activates its content. A workspace opts in with
an `extension:<name>` entry in `agentArtifacts`; see the CLI's
[agent-workflow lifecycle](../../../tooling/cli/doc/adr/0047-extension-owned-agent-content.md).

### Superseded artifacts

When an extension's content replaces workflows that were distributed as
separate agent artifacts, `supersedes` names those artifact identities. It is
optional, belongs to both forms, and the package step keeps it:

```json
{
  "name": "@acme/contributor",
  "agentContent": {
    "path": "agent-content",
    "source": "agent-src",
    "supersedes": ["@acme/agent-workflows", "@acme/maintainer-workflows"]
  }
}
```

Each entry is a registry name (`@scope/name` or `name`), appears once, and is
never the extension's own name (`invalid-agent-content-supersedes`). A
workspace that opts into this content while it still declares, or still records
as installed, a superseded artifact is refused instead of receiving two
competing sets of instructions. See
[ADR 0006](adr/0006-agent-content-is-an-additive-contract.md).

## Option Namespaces

A project's `putnami.json` is addressed to several extensions at once. Two
spellings are attributed automatically: `options.<extension name>` and
`options.<path reference>`, each optionally with a `:<command>` suffix. A bare
block — `options.sdd` — is attributed to nobody, so `optionNamespaces` is where
an extension says it reads one:

```json
{
  "name": "@putnami/sdd",
  "optionNamespaces": ["sdd", "publish"]
}
```

Declaring a namespace never widens this extension's cache keys; it narrows every
OTHER extension's. A task's config digest drops a namespace another manifest
declared and its own did not, so an `options.sdd` edit stops re-running the Go
build of the project that carries it. Three rules keep that safe:

- a namespace no manifest declares stays in every key — attribution is declared,
  never inferred from a name;
- a namespace two manifests declare stays in both — it is an input of both;
- a command name is never attributed, whatever a manifest lists: the CLI merges
  `options.<command>` into the resolved parameters of every extension providing
  that command.

Declare a namespace where it is READ, not where it is written: the project that
authors `options.sdd` does not decide who consumes it.

## Hook Invocation Order

Several extensions can declare the same hook kind for one project. They are
invoked in ascending `hooks.<kind>.order`, ties broken by extension name in
byte order, so the sequence is a pure function of the manifests — never of map
iteration or filesystem order.

| `order` | Meaning |
|---------|---------|
| `0` (default) | Producer: the hook only writes generated sources and reads nothing another hook generated. |
| `> 0` | Consumer/finalizer: the hook must observe what the producers generated (for example it imports the workload entry point, or inventories the whole generated tree). |

`extensionDependencies` is a *library* relation and deliberately does not
imply a hook order: an extension can depend on another's API while its hook has
to run first, which is exactly the case for `@putnami/web` (it depends on
`@putnami/application` but generates the server loaders that
`@putnami/application`'s hook then loads).

## Extension Dependencies

`extensionDependencies` is a list of names or a map from name to version
constraint. The CLI acts only on a registry name (`@scope/name`). For an
extension installed from the artifact store or declared by path, and for a
declared name the lock pins, `putnami install` installs each registry
dependency, pins it in `putnami.lock.json`, and discovery loads it. Any other
name is left to the package manager.

A dependency the workspace does not declare loses every reserved provider
command: credential, cache and runner providers, session and log reporters,
and the Cloud release set. Declare an extension in `putnami.workspace.json`
when the workspace must use one of its providers.

A manifest that names a registry dependency requires CLI contract 8
(`ExtensionDependenciesContract`) when the CLI loads it from the artifact
store or a declared path. The Go archive packager stamps 8 on it, even when
it declares only hooks. The npm packager leaves the list out of its stamp, so
an npm package that declares only hooks stays unstamped.

## Extension Discovery

Extensions are discovered from:
1. Workspace-level entries in `putnami.workspace.json` → `extensions`
2. Scope-level entries in scope `putnami.json` → `extensions`
3. The dependencies of those extensions that the lock pins

No `node_modules` directory and no `package.json` is a source of extensions.

Use `putnami extensions list` to see all discovered extensions.

## Validation

Validate an extension manifest:

```bash
putnami extensions validate [path]
```

This checks:
- JSON Schema conformance
- All pipeline steps reference defined tasks
- Pipeline DAG is acyclic
- Activation file patterns are valid globs
- Contract schema references are valid

## Schema

The formal schema is in [`schemas/extension.json`](../schemas/extension.json).
