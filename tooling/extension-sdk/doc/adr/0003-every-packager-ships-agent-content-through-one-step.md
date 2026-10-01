# ADR 0003 — Every packager ships agent content through one step

- **Status**: accepted
- **Scope**: `putnami-extension-sdk` (`tooling/extension-sdk/agentartifact`), the
  contract gate shared by `@putnami/go` and `@putnami/typescript`
  (`manifest_contract.go`), `@putnami/scaffold`

## Context

An extension may declare `agentContent`
([`protocols/extension` ADR 0006](../../../../protocols/extension/doc/adr/0006-agent-content-is-an-additive-contract.md)).
The authored form names a source directory; the packaged form binds the built
tree by the digest of its content manifest, and a manifest carrying it needs
`cliContract` 5. Three packagers ship extensions:
`agentartifact.PackageExtension` for a content-only extension
([scaffold ADR 0004](../../../scaffold/doc/adr/0004-one-packager-for-every-content-project.md)),
and the Go archive and npm packagers for an extension with an executable. All
three must emit the same content bytes and the same stamp.

## Decision

### 1. One build, staged before the gate

`agentartifact.StageExtensionContent(extensionRoot, stageDir, name, version)`
is the only way a language packager ships agent content. It builds the authored
source with `BuildExtensionContent` and the declared content policy, writes the
tree under `agentContent.path` in the stage with fixed modes, and rewrites the
staged manifest to the packaged form, keeping `supersedes`. A manifest without
`agentContent` is not read past that field and its bytes are not touched.

The Go packager calls it once, on the shared base stage, before any platform
copies it, so every platform archive carries the same content by construction.
The npm packager calls it on the package directory.

### 2. The stamp is what the vocabulary needs

The shared gate stamps `extension.RequiredCLIContract(m)`: 5 for a manifest with
agent content, 4 otherwise. It refuses a claim above `LatestContract`. A claim
at or below it is replaced by the earned stamp, so an author's `5` on a manifest
without agent content ships as `4`. `PackageExtension` applies the same rule.

### 3. The postcondition covers the content

The staged artifact must load *and* ship exactly the content it binds:
`VerifyStagedExtensionContent` requires the packaged form, the content
manifest's digest, every declared file's digest, no undeclared file and no
symlink. A packager that skips step 1 fails here.

### 4. What staging refuses, before it writes

- a contribution without `source`: a committed digest would bind bytes nobody
  rebuilt;
- a manifest `name` that differs from the published name: the CLI binds content
  to the name it resolves;
- an `agentContent.path` the stage already holds, or one below a staged file or
  symlink;
- content the declared policy or the closed layout rejects.

The npm packager also refuses a `package.json` `files` allowlist that omits the
content path, because `npm publish` applies it after the gate.

## Rejected alternatives

- **Build the content per platform.** One build per platform, and identical
  content becomes a property to test instead of a fact.
- **Accept a committed packaged form.** Its digest names bytes the package step
  never built.
- **Stamp contract 5 on every manifest.** Every contract-4 CLI would refuse
  extensions that use no contract-5 vocabulary.
- **A content step per packager.** A third copy of the emission and binding
  rules.

## Consequences

- An extension with commands, tools or a runtime ships its instructions in the
  same archive or package, under the same version, as its executable.
- `TestManifestContractGateHasNoTwinDrift` keeps the two gates identical, and
  both call the one SDK step.
- How the CLI reads installed npm content is
  [CLI ADR 0047](../../../cli/doc/adr/0047-extension-owned-agent-content.md).
