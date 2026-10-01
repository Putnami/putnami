# ADR 0002: Lines are scopes, registries are keyed by ecosystem, versions come from git

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): the
  `putnami.workspace.json`, scope, and project documents

## Context

Putnami releases and versions TypeScript, Go, Python, and tooling separately,
and derives versions from git. Extensions own their ecosystems and must work
with the outside world without Putnami Cloud, so each ecosystem's registry
configuration lives in one place.

## Decision

1. **A version line is a scope that declares a `line` block.** The block carries
   the pattern of the line's git tags, with `{version}` as the only placeholder.
   An empty block means `<scope path>/v{version}`. A workspace without any
   `line` block is one line with the pattern `v{version}`. A project's line is
   its nearest ancestor scope with a `line` block; a line nested under another
   line is a validation error. `line` is not inherited by merge.
2. **No document carries a `version` field.** The CLI derives a commit's
   version from git: the tag's version on a tagged commit, otherwise the line's
   next version computed from conventional commits, with the ordered suffix.
3. **`registries` is a workspace section keyed by ecosystem.** Each entry's
   schema is owned by the ecosystem profile the extension declares; the
   workspace schema validates only the outer map. A project may override an
   entry in its own document. The section is the single source of every
   registry endpoint; the CLI and extensions generate native files, `.npmrc`
   among them, from it.
4. **The workspace document has no `distribution` section.** Release-set
   provider, namespace, channels, mirrors, and environments live in
   `putnami.ci.json`. Who may pull a published artifact is the
   `distribution.visibility` block of a project or scope `putnami.json` (the
   project wins, then the deepest scope), so making one package public is a
   change to that package.
5. **One selection vocabulary** everywhere a document selects projects:
   `tag:<tag>`, `group:<group>`, `scope:<path>`, or a project id.

Scope with a line, and workspace registries:

```jsonc
// typescript/putnami.json
{ "line": { "tag": "ts/v{version}" } }

// putnami.workspace.json
{
  "registries": {
    "npm": { "publish": "https://npm.putnami.dev", "scopes": { "@putnami": "https://npm.putnami.dev" } },
    "go":  { "origin": "https://go.putnami.dev", "proxy": ["https://proxy.golang.org", "direct"] },
    "oci": { "publish": "oci.putnami.dev/putnami" }
  }
}
```

## Consequences

- `putnami version tag --scope <line>` computes the line's next version,
  regenerates its changelog, creates the release commit, then the annotated tag
  from the line's pattern. There is no `version set` or `version bump`.
- `putnami scopes list` reports each scope's line and tag pattern.
- Extensions read registry endpoints from `registries.<ecosystem>`;
  `publish.dockerRegistry` and `publish.goRegistryUrl` are not workspace fields.
- A root tag `v<version>` is refused in a workspace that declares lines.
