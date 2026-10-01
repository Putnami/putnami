# Distribution

Extensions can be distributed as standalone packages, installed into workspaces, and shared across teams.

## Packaging

Package an extension for distribution:

```bash
putnami extensions package [path]
```

This creates a distributable archive containing:
- `putnami.extension.json` manifest
- Task scripts and binaries
- Template files (if any)
- Integrity checksums

The package is validated before creation — packaging fails if the manifest doesn't pass validation.

A content-only extension — one that declares `agentContent` and runs nothing —
is packaged by the SDK's `agentartifact.PackageExtension`: it builds the
authored `source` with the agent-artifact builder under the extension's own
name and version, applies the declared content policy, writes the built tree
under `agentContent.path`, stamps the packaged form (`path` plus
`manifestSha256`, never the source) and `cliContract` 5, and refuses to return a
manifest this CLI's own loader would not read. The archive is platform
independent: the same reproducible bytes serve every host. `@putnami/scaffold`'s
`package` job runs this step and writes the archive under every registry
platform key.

An extension that ships an executable beside its agent content is packaged by
its language extension (`@putnami/go` archives, `@putnami/typescript` npm
packages). That packager stages the content with the SDK's
`agentartifact.StageExtensionContent` — the same builder, policy and packaged
form — before its contract gate, which stamps `cliContract` 5 only for a
manifest that declares `agentContent` and refuses a stage whose content bytes
differ from the digests the manifest binds. A Go extension's content is staged
once, so every platform archive carries identical content bytes.

## Installation

Install an extension into a workspace:

```bash
putnami extensions install <source>
```

Sources:
- **Local path**: `putnami extensions install ./my-extension/`
- **Workspace reference**: Extensions listed in `putnami.workspace.json` → `extensions` are auto-discovered
- **Scope reference**: Extensions listed in scope `putnami.json` → `extensions` are inherited by all projects in the scope

### Cross-platform materialization

Archives are per-`os`/`arch`, and the lock records a digest per platform. To materialize a platform other than the invoking machine's — packaging a warm artifact store into an image, say — pass the target explicitly:

```bash
putnami extensions install --platform linux/amd64 --dest ./.gen/warm-artifacts
```

The archive is fetched for the named platform and verified against that platform's entry in `putnami.lock.json` → `integrities`; `--dest` receives a drop-in artifact-store root (`<dir>/sha256/<xx>/<digest>/`). Because the result is packaging output rather than an install, install hooks are skipped, the stable links and the lock file are left untouched, unverifiable bytes are a hard error, and the tree is normalized so the same lock and platform reproduce byte-identically.

## Discovery

Extensions are discovered from multiple locations:

1. **Workspace config**: `putnami.workspace.json` → `extensions` array
2. **Scope config**: Scope `putnami.json` → `extensions` array (inherited by projects)
3. **Project config**: Project `putnami.json` → `extensions` array (per-project override)
4. **Node modules**: `devDependencies` in `package.json` that contain `putnami.extension.json`

Use `putnami extensions list` to see all discovered extensions, their paths, and provided commands.

## Workspace References

Extensions within the same workspace use path references prefixed with `/`:

```json
{
  "extensions": [
    "/typescript/extension",
    "/go/extension",
    "/python/extension"
  ]
}
```

The path is relative to the workspace root.
