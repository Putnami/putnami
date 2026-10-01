# Caching & Dependencies

Go jobs are cache-aware and keep Go module metadata aligned with workspace dependencies.

## Cache inputs

| Job | Inputs |
|-----|--------|
| `build` | `**/*.go`, `go.mod`, `go.sum`, `go.work`, `package.json` |
| `test` | `**/*.go`, `go.mod`, `go.sum`, `go.work`, `package.json` |
| `lint` | `**/*.go`, `go.mod`, `go.work`, `.golangci.yml`, `.golangci.yaml`, `package.json` |
| `serve` | depends on build, never cached |

## Go build cache

The task cache above decides whether a job RUNS. Inside a job that does run, the
`go` command keeps its own cache of compiled packages, and that one is machine
local: `<cache root>/build` by default.

When the run is served by a cache provider, the extension points Go's
`GOCACHEPROG` at `putnami-go gocacheprog`, a helper that answers from
`<cache root>/prog` first and asks the provider's object cache on a miss. A
machine that starts cold is then served the packages another machine already
compiled, instead of rebuilding the standard library and every dependency from
zero. Uploads happen behind the compiler, a missing or slow provider costs the
build nothing, and `--remote-build-cache=false` turns the whole thing off for a
run.

See the extension's [remote build cache](../../../extension/doc/build.md#remote-build-cache)
for the trust rules and for what is shared between machines.

## Dependency sync

Build can refresh dependencies unless `--skip-deps` is set. That keeps:

- `go.mod`
- `go.sum`
- workspace dependency metadata

aligned with the Putnami project graph.

## Final branch check

Use the workspace-level impacted command before merging:

```bash
putnami lint,test,build --impacted
```
