# reportstream fixtures

Every `.jsonl` here is a REAL captured `putnami … --output=jsonl` stream, never a
hand-written one: the parser's whole job is to survive what the CLI actually
emits, so a fixture nobody's CLI produced would pin nothing.

## `lint-test-build.jsonl` — putnami v1 (frozen contract)

A trimmed real capture of a `putnami lint,test,build --output=jsonl` run in a
multi-project workspace (2026-07-03, 96 lines), with project names and paths
replaced by neutral ones (`libs/cli`, `libs/internal/apitest`, `libs/runtime`,
`apps/gomod-server`). It covers the four v1 envelopes
(`job:start` / `job:event` / `job:end` / `session:end`), every `job:event` type
the parser folds, and a mixed set of task verdicts (success / skipped / failed /
canceled). It stays checked in because v1 remains supported for older CLIs until
putnami removes the v1 emitter.

## `warm-run.jsonl` — a warm CI run

A real capture of a fully cached CI gate run over two projects (`apps/console-ui`
and `apps/console`), 31 lines. The Putnami stream sits between the runner's
`ci.runner.start` and `ci.runner.end` markers. All 14 jobs are cache hits, so
the stream carries `job:start` / `job:end` records but no `job:event` test or
coverage data (`TestWarmFixturePinsCachedResultContractGap`).

## `v2-lint-test-build.jsonl` + `v1-twin-lint-test-build.jsonl` — the v1/v2 pair

Two captures of the SAME command, run back to back against the same tree with
the same CLI, one per contract. They are what pins the v2 mapping: the batch
folded from the v2 capture must equal the batch folded from its v1 twin
(`TestV2BatchEquivalentToV1Twin`).

- **CLI**: `Putnami/putnami` @ `405dcd5426e64188b4b6d9b13ce6c5b425e0aa58`
  (`0.1.0-405dcd542`), the first default-v2 CLI.
- **Workspace**: the `Putnami/putnami` framework checkout itself (its own
  `protocols/cli` project is a small, fast, self-contained selection).
- **Date**: 2026-07-28. **Exit code**: 0 for both. **91 lines each.**
- **Commands** (`--no-cache` so both runs execute rather than replaying cache
  hits, which the capturing CLI did not replay as structured `job:event` /
  `task:event` records):

  ```sh
  # v2 (the CLI default since 0.1.0-405dcd542)
  PUTNAMI_NO_AUTO_INSTALL=1 putnami lint,test,build \
      --projects go.putnami.dev/protocol/cli --output=jsonl --no-cache

  # v1 twin, via the PUTNAMI_MACHINE_OUTPUT rollback env var
  PUTNAMI_NO_AUTO_INSTALL=1 PUTNAMI_MACHINE_OUTPUT=v1 putnami lint,test,build \
      --projects go.putnami.dev/protocol/cli --output=jsonl --no-cache
  ```

Both files are unedited stdout. Nothing is normalized: timestamps, durations
and the concurrent scheduler's completion order are real, so the equivalence
test compares build tasks as a sorted, duration-free multiset and the golden tests assert only run-stable fields
(project, task, status, cache hit, counts).

Regenerating them means re-running both commands and re-deriving the counts the
tests assert — do not patch a line by hand.

## `verbose-session.jsonl` — one session's stdout and its session record

A real capture of a small v2 run under the verbose budget, so the live copy
elided nothing (`machineOutput.elided` is zero for both classes). 158 lines.

- **CLI**: the framework checkout's `./putnamiw`, built from
  `95ef1b42bccabc8bfe8ed4f937396c6225d0669f`, with uncommitted test and spec
  changes only.
- **Workspace**: the framework checkout itself.
- **Date**: 2026-10-27. **Exit code**: 0. **Session**: `20261027-231809-0e209a`.
- **Command**:

  ```sh
  GOFLAGS=-buildvcs=false ./putnamiw test \
      --projects go.putnami.dev/protocol/gomod --output=jsonl --verbose --no-cache
  ```

The stdout and `.putnami/sessions/20261027-231809-0e209a/events.jsonl` were
byte-identical (same SHA-1, `8af71f05…`), so the one file serves as both copies
of the session: `FoldRun` reads it as stdout and as the session record
(`artifact_test.go`). The elision tests drop its `task:event` and `test:case`
records in memory to stand for a stdout that ran out of budget; the file
itself is unedited.

## `twins/`

The CLI-side goldens of the delivery-records report payload contracts, written
by `go test -run TestReportTwins -update-twins`. The server side keeps identical
copies and validates them against the contract schemas; see `contract_test.go`.
