# Toolchain & Detection

The Go extension activates for Go projects in the workspace and resolves the Go toolchain automatically.

## Project detection

Go projects are selected when the project contains Go source or module metadata, most commonly:

- `go.mod`
- `*.go` files
- tests using the Go `*_test.go` convention

Templates create the expected shape:

```bash
putnami projects create api --template go-server
putnami projects create domain --template go-library
```

## Toolchain resolution

The extension resolves Go in this order:

1. workspace `go.work` directive
2. project `go.mod` directive
3. compatible Go from `PATH`
4. managed install under `~/.putnami/toolchains/go/go-<version>/`, shared by
   every workspace of the machine
5. latest stable Go when no version is pinned

`GOCACHE` and `GOMODCACHE` are shared at `~/.putnami/cache/go` across every
repository and worktree on the machine. Go's content keys isolate toolchains,
targets, flags, and sources, so concurrent worktrees reuse compiled packages
without mixing incompatible results. Set `PUTNAMI_GO_CACHE_DIR` to relocate the
cache. The extension owns the cache's lifecycle: `putnami cache gc` fans out to
its `cache-gc` command, which enforces the machine-wide 10 GiB budget, and
`putnami cache clean` resets the build cache for a cold rebuild.

When a cache provider serves the run, the extension also points `GOCACHEPROG` at
its own helper so compiled objects are shared through the remote cache instead of
being rebuilt on every cold machine. The helper keeps its objects in a third
directory under the same root, `<root>/prog`, which both cache commands collect
alongside `build`. See the extension's
[remote build cache](../../../extension/doc/build.md#remote-build-cache).

## Scope

The extension owns Go-specific jobs. Putnami still owns project selection, dependency ordering, cache reuse, and impacted-project detection.
