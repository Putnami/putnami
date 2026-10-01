# Exact dependency documentation

The Go extension contributes the read-only MCP tool `putnami.go_docs`. Agent
hosts use it when they need API documentation for a module selected by an
application:

```json
{"reference":"go.putnami.dev/app","project":"services/api"}
```

`reference` is an exact Go module path. `project` is optional when precisely
one Go project in the workspace declares that module; otherwise pass a Putnami
project name, module path, or workspace-relative directory. The workspace root
comes from the MCP session and is not a tool argument.

The Go binary is chosen by the extension's one toolchain resolver — the same
one `build`, `test` and `lint` use — so a workspace whose Go is managed by
Putnami rather than installed on `PATH` answers documentation reads normally.
The project being asked about supplies the version requirement, which matters
because the read forces `GOTOOLCHAIN=local`: a toolchain older than that
project's `go` directive is rejected up front instead of failing opaquely inside
`go list`.

The extension delegates selection to `go list -m`. It preserves an explicit
human `GOWORK` choice, including `GOWORK=off`, and otherwise selects the
project's nearest governing `go.work`. It forces `GOPROXY=off`, `GOSUMDB=off`,
`GOTOOLCHAIN=local`, and `-mod=readonly`, so a documentation read cannot fetch a
module or edit `go.mod`, `go.sum`, `go.work`, or `go.work.sum`. Workspace modules
and local replacements are reported as `workspace-replacement`; cached module
sources are `installed-package`.

An available result includes the exact package and selected version, the
resolved `AI.md` path, a SHA-256 digest of its bytes, and the content. Reads are
limited to 1 MiB and reject a documentation symlink that leaves the resolved
module directory. Missing cache state returns `offline_missing`; a selected
module without safe bounded documentation returns `docs_missing`; an invalid
reference or ambiguous project returns `unresolved`. These are successful tool
responses with a fallback, so an agent can continue from local source without
retrying or triggering installation.

The `framework-docs/` directory carried by the extension remains a general
identity reference. The tool never labels those unversioned copies as the
documentation of an application's selected module.
