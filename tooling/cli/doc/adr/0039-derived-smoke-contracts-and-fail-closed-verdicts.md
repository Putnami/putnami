# ADR 0039 — Derived smoke contracts and fail-closed verdicts

- **Status**: accepted
- **Scope**: `putnami qualify` (`tooling/cli/internal/qualify`), the
  `protocols/qualify` contract, and its TypeScript twin in
  `@putnami/cli-protocol`

## Context

`putnami lint,test,build,validate` proves a change statically. It does not
prove that a workload boots through its production entrypoint, reaches
readiness and serves a request. Without a shared mechanism each proof is an
improvised curl or database, unreviewable and silent about which build it ran
against. A purely local proof also misses the failures only a deployed artifact
exposes, such as an uncaptured `.gen` asset.

## Decision

1. **The smoke is data derived from existing contracts, not an authored
   scenario.** The CLI reads the workload's `putnami.http-routes.v1` inventory
   (`schema/http-routes.json`, then `.gen/schema/http-routes.json`) and derives
   one request per exact, `publicEdge` route that accepts `GET` or `HEAD`,
   excluding the platform endpoints (bare or under the platform prefix),
   `/debug/pprof` and `/_putnami/`. Requests are sorted by path and capped at
   25; each passes below status `500`. Read-only requests are the only set that
   is safe by construction against any environment.
2. **Nothing derivable is `unsupported`, never `passed`.** An empty contract
   opens no target; its verdict is `unsupported`, with a remedy
   (`putnami build --projects <id>` for a missing inventory).
3. **A verdict binds to the build it proved.** A URL target requires
   `--expect-sha` (at least 7 hex characters), and `/version` must report a sha
   that starts with it. A different or absent sha is `digest_mismatch` and no
   smoke request is sent. `--target local` composes the workload in production
   mode without watch and reads the worktree fingerprint before the serve
   pipelines are prepared and again once every member is ready. A changed tree
   is `digest_mismatch`: two equal readings around the build prove the workload
   was built from that tree.
4. **The verdict vocabulary is closed and fails closed.** `passed`, `failed`,
   `unsupported`, `not_run`, `timed_out`, `canceled`, `target_unreachable`,
   `digest_mismatch`, `composition_failed`. Only `passed` exits `0`; every other
   state exits `1` and still delivers the verdict (the failure envelope's
   `data` under `--output=json`). Usage errors exit `2`.
5. **The wire shapes are a protocol with a TypeScript twin.**
   `protocols/qualify` owns the contract and verdict types, schemas and a
   shared fixture corpus; `@putnami/cli-protocol` validates the same corpus
   with the same codes. The verdict travels as the `data` member of the
   result-v2 envelope; `result_v2.go` and `session.json` gain no member. A
   strict reader refuses a `passed` verdict without its proof: every phase
   passed, at least one request and every request passed, an observed sha
   starting with the expected one, and a clean teardown.

## Consequences

- A workload whose only public routes mutate state, or that declares no
  inventory, cannot be qualified; it shows as `unsupported`, not as a pass.
  Declared-safe mutations would need a safety annotation on the inventory in
  both producers.
- A deployment that does not stamp its sha on `/version` cannot pass. A local
  verdict never reads `/version`, so a source build without a sha still
  qualifies locally.
- A local run needs a git worktree, and fails when its preparation, workload or
  editor writes a file git does not ignore.
- `qualify` waits on live workloads, so MCP `run_jobs` refuses it and the
  portable runner never carries it.
- A structured command that declares positionals, emits structured output and
  has no subcommands keeps its first bare argument as a positional
  (`commandmeta.PositionalLeaf`), so `qualify <project> --output=json` resolves
  to the `qualify` catalog row.

## Rejected alternatives

- **Authored smoke manifests per workload.** A second, hand-maintained
  description of the surface drifts from the code; the generated, validated
  inventory already exists.
- **Health-only smoke (`/readyz`, `/healthz`).** Readiness proves a process is
  up, not that it serves a business request; a broken router or a missing
  `.gen` asset leaves the platform endpoints working.
- **A `session.json` member for the verdict.** No session reader consumes the
  verdict, and the result-v2 drift tests lock three representations; the
  envelope's `data` carries it.
