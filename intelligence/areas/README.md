# Area detector

`go.putnami.dev/intelligence/areas` splits a repository into areas: the parts
an agent works in. It reads the repository's own manifests, so the repository
does not need to be a Putnami workspace.

`Detect` takes the tracked file paths and a `Reader` that returns file contents.
It returns a `Layout`:

- Areas a workspace manifest declares, at the root or in a top-level directory:
  pnpm, npm workspaces, Lerna, Nx, `go.work`, Cargo, uv, Maven, Putnami, and
  Bazel packages.
- Inferred areas for code that no manifest declares: the directories that own
  a language manifest, then the top-level directories.
- The root area, for files outside every other area.

Each area has a `Source` (`manifest` or `inferred`) and a `Role` (`code`,
`tests` or `docs`). Lockfiles, minified bundles, vendored dependencies and
build output never shape an area.

The module is not published. No project in this repository imports it yet.
