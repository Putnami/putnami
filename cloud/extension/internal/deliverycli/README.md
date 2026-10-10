# Delivery CLI

This package holds the Delivery commands of the `@putnami/cloud` extension,
in package `deliverycli` (`go.putnami.dev/cloud/extension/internal/deliverycli`).
The aggregator in [`internal/cloudcli`](../cloudcli) imports it.

- `putnami cloud ci ...` (`ci.go`): run operations, recovery modes and the
  subscription. `putnami cloud ci report` (`report.go`) submits a delivery-run
  conclusion.
- `putnami cloud ci init|validate|fmt|explain` (`ci_contract.go`): write,
  check, format and explain `putnami.ci.json`. They parse it with the pinned
  [`go.putnami.dev/protocol/ci`](../../../../protocols/ci) module. `validate`
  also reads the commands every discovered extension declares, so it reports a
  job no extension serves, and refuses `distribution` and `envs` when no
  extension serves release sets.
- `putnami cloud channels follow <namespace> <channel|rs_id>` (`subscription.go`):
  changes what the workspace's CI follows.
- `putnami cloud cache` (`cache.go`), and the `cache-provider` and
  `credential-provider` processes the engine starts. The remote build-cache
  client lives in [`internal/remotecache`](internal/remotecache).
- The `session-reporter` (`sessionreporter.go`) and `log-reporter`
  (`logreporter.go`) processes the engine starts. They post each
  session-reporting chunk to the Delivery ingest under the run credential.
  The session reporter also posts `plan.json` as the `plan` part; the log
  reporter refuses it.
- `status_node.go` builds the status node of `putnami cloud ci status` and
  `putnami cloud cache status`. `CIStatusNode` and `CacheStatusNode` are the
  same nodes for the `ci` and `cache` lines of `putnami cloud status`.

## Reporter credential

A hosted run (`--credential-fd`, framework CLI 0.4.0 and later) hands each
reporter the run credential over session reporting protocol 2
(`reporterhandshake.go`). The engine sends `initialize`, the reporter accepts
it, then the engine sends `authenticate` with the credential. The reporter
keeps the credential in memory and uses it as the bearer of every chunk. A
reporter that could not deny process inspection refuses `initialize`, so it
never receives the credential. A malformed or oversize credential is refused
with `invalid_run_credential`.

Without a run credential the engine sends no handshake. The first line is a
chunk, and the reporter uses the token the engine placed in its environment
(`PUTNAMI_SESSION_REPORTER_TOKEN` or `PUTNAMI_LOG_REPORTER_TOKEN`), as under
protocol 1. A credential from the handshake wins over that token.

The two reporters post their chunks with hand-written HTTP, because
delivery-api's contract can describe the octet body of a chunk but not the
frame's identity. Both call sites
are listed in [`clientgen.framework.json`](../../clientgen.framework.json).

The rest of this file covers the local proofs.

## Local proofs

The runner handover helper has a subprocess proof
(`runner_next_subprocess_test.go`) that invokes the built Cloud CLI's real
`__runner-next` entrypoint against a fake of delivery-api's
`POST /api/ci/runner/next` route. The fake lives in
`runner_next_provider_test.go`. It reads and writes the generated client's
types, and keeps the route's request checks and its purpose-scoped credential
checks. The test scripts the job assignment; the Delivery API's own
integration suite covers the real route, admission and saga behavior.

Run the proof in two steps from the repository root:

1. Build the CLI:

   ```sh
   ./putnamiw build --projects @putnami/cloud-extension
   ```

2. Pass the absolute path of the **host-platform** binary to the focused test.
   For example, on macOS arm64:

   ```sh
   PUTNAMI_RUNNER_NEXT_TEST_BINARY="$PWD/.putnami/out/cloud/extension/build/bin/darwin-arm64/putnami-cloud" \
     ./putnamiw test --projects @putnami/cloud-extension \
     --run '^TestRunnerNextBuiltCLIAgainstProviderFake$' --no-cache --count 1 \
     --no-enforce-coverage --test-verbose
   ```

   Use `linux-x64` in place of `darwin-arm64` on Linux amd64.

The test checks that the binary is the `cmd/putnami-cloud` production
entrypoint and targets the host platform, then runs it with an empty `PATH`
and an isolated home. Valid host aliases produce exactly the assigned env
file; ordinary job credentials are denied, and idle or denied calls write
nothing to stdout or stderr. Replayed assignments retain fresh valid ingest
aliases.

Without `PUTNAMI_RUNNER_NEXT_TEST_BINARY` the subprocess proof skips. The
in-process provider tests (`runner_next_test.go` and
`runner_next_provider_test.go`) still run normally.
Keep this opt-in proof uncached and build first: a test cannot take the
project's own build output as an input. The focused command disables coverage
enforcement because it selects one integration test; the ordinary full gate
owns coverage enforcement.

## Publication-v1 provider proof

On a hosted run the credential provider also serves the engine's
publication-v1 ops: `resolve`, `open`, the publish credential, and `release`
(see [registry ADR 0003](../../../../protocols/registry/doc/adr/0003-publication-ops-behind-a-negotiated-capability.md)).
`publication_test.go` and `publication_advance_test.go` drive the provider
over its JSONL protocol, the way the engine does, and check every answer with
the protocol's strict parsers and exchange validators. Delivery's
`/native-publication/planning`, `/native-publication/publish` and
`/native-publication/advance` routes and put-server's
`/put/_/release-sets/resolve` and `/release` routes are `httptest` doubles:
the Delivery double decodes the plan the way Delivery does and recomputes its
digest, and the put-server double moves channels by compare-and-swap. The
provider reaches put-server through its generated client. The tests need no
database and run in the ordinary gate:

```sh
./putnamiw test --projects @putnami/cloud-extension --run 'Publication|ReadRequestLine|ParsePlanningGrant|ParsePublishGrant|ParseAdvanceAnswer|PreservesHead|WithChannels'
```

Forward-only channels follow one rule. The provider never refuses with
`not_forward`: `open` keeps the engine's ancestry statement without judging
it, so every run uploads its artifacts. Before a release that names a
forward-only channel, the provider asks Delivery's `advance` route whether the
run is still the head of its default branch:

- Not the head (a superseded run, or a rerun of an older commit): the
  forward-only channels stay where they are, the other channels are released,
  and the engine reads `already-current` (or `released` when another channel
  moved). One stderr line names the skipped channels and Delivery's head.
- The head: the provider reads the channels again and moves each
  forward-only channel from the head it carries now. A head that carries a
  member this run neither rebuilt nor carries at that artifact is a
  `conflict`, so an older artifact never returns to the channel. A head that
  moves again before the move is read once more, Delivery is asked again, and
  a second miss is the `conflict`.
- Delivery not answering moves no channel and stores nothing, so the engine
  can send the release again.

A run whose immutable channel already has a head is a rerun. When the tag's
channel already has a head at `open` (a rerun of a commit whose run released
it), the plan still opens and its artifacts upload. The engine resolves no tag
channel, so the provider reads it at `open` when the session did not. The
release then moves no channel, mutable ones included: put-server and the
`advance` route are not called, and the engine reads `already-current` with
the set as the head of every channel. A tag rerun therefore never moves
`stable` back to an older tag. When put-server refuses a release with
`channel_immutable` because a concurrent run of the same tag created the
channel after `open`, the provider reads the channel once more and, when it
now has a head, answers the same way; otherwise the refusal stands. A plan
that names an existing immutable channel as mutable is a plan error:
put-server refuses it with `channel_immutable`.

The provider does not skip a channel that is neither forward-only nor held on
the engine's ancestry statement: a pull-request channel would stay frozen after
a force-push.

Locally, without a run credential, the provider does not echo publication-v1,
and the engine keeps its own publication path. A hosted run whose launcher
exported no `CI_LIBRARY_PUBLISH_*` origin does not echo it either, and prints
nothing about it on stderr.

The provider does not verify artifact digests and never refuses with
`artifact_digest_mismatch`. put-server's release check refuses, as
`artifact_missing`, a newly introduced Put or archive member that the Put
registry does not store at its digest, and checks no npm, Go or OCI member.

## Server requirements

Publication-v1 needs a delivery-api that serves the
`native-publication/advance` route. An older delivery-api answers 404, which
the provider turns into a final `publication_refused`, so every
default-branch publish run fails. Enable the publish purpose with the engine's
`--providers` flag only against a delivery-api that serves the route.
