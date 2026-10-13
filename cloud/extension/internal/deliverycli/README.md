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
  The session reporter also posts `plan.json` as the `plan` part and the spec
  gate's record, `spec-verification.json`, as the `spec-verification` part;
  the log reporter refuses both. A receiver that predates a part answers 400,
  and the engine omits that document.
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

## Cache provider

The `cache-provider` process moves a run's task entries between core and the
cache server. Core is the Putnami build scheduler: it starts the provider and
sends it one request per line. `cacheprovider.go` holds the session,
`cacheupload.go` the uploader, `cachedrain.go` the wait at the end of a run,
and `internal/remotecache` the HTTP client. This section gives the bounds the
provider applies.

### Uploads

`upload` queues an entry and answers at once. One uploader stores the queue a
window at a time. A window costs one `commit-batch`, one `upload-batch` per
4 MiB of small blobs, and a `find-missing` when it has blobs to PUT, so a
window is as large as one `commit-batch` takes. It is stored as soon as it
reaches one of these bounds:

| Bound | Value |
| --- | --- |
| Entries in a window | 5,000 |
| Distinct blobs in a window | 5,000 |
| Encoded entries in a window | 512 KiB |
| Size of the distinct blobs in a window | 256 MiB |
| Blobs or entries named in one request | 5,000 |
| Blob PUTs at once | 16 |

A window that reaches no bound is stored once no entry has arrived for 30 s,
and at the end of the run: the summary stores every queued entry. A CI run
that builds a few hundred entries stores them in one or two windows.

An entry with more than 5,000 distinct blobs goes alone. Its blobs are asked
about in several `find-missing` requests of at most 5,000 each. `commit-batch`
and `download-batch` are split the same way.

**The first write carries no blob bytes.** Until the server has accepted one
write of the session, the provider sends a `find-missing` before any
`upload-batch`. A credential that only reads gets its 403 there, and the write
path closes for the run. This costs at most one small request per session, and
none once a write is accepted.

**A failure costs as few entries as it can.**

1. A `find-missing`, `upload-batch` or `commit-batch` that fails with a 5xx, a
   429, a 408 or a network error is sent again: 3 attempts in all, after waits
   of 0.5 s and 2 s. Each attempt has 30 s.
2. A 403 is never sent again: it closes the write path at once. A 401 keeps
   the one replay it had, with a new bearer, when the bearer's source can
   mint one. Another 4xx is not sent again.
3. A blob PUT that fails is not sent again. It fails the entries that name the
   blob. The other entries of the window are committed.
4. A window that still fails as a whole is stored entry by entry, once, with
   one attempt each. The split stops after 3 entries in a row fail for a
   reason that can pass.

An entry is committed, and counted in `uploadedCount`, only when all its blobs
are in storage.

**A blob PUT has a deadline:** 60 s, plus 1 s for each 256 KiB of the body it
sends. A blob that gzip shrinks is timed on its compressed size.

| Body | Deadline |
| --- | --- |
| 1 MiB | 64 s |
| 20 MiB | 140 s |
| 100 MiB | 460 s |
| 1 GiB | 69 min |

### Restores

`prefetch` negotiates the run's keys once, when the run starts. Then it
fetches the small files of every hit, the files of at most 256 KiB, through
`download-batch`:

- It asks only for files core does not have and will read. It asks for no
  file of a key core restores for its result only.
- One request asks for at most 5,000 files and 4 MiB of them, so its answer
  stays near 5.4 MiB: cache-server bounds each file, not the whole answer.
  4 requests run at once.
- A restore that needs one of those files waits for the request that carries
  it, then finds the file in the exchange directory. A run sends a few
  `download-batch` requests, not one per restored entry.
- When that request fails, each restore asks for its own small files in one
  `download-batch`, as before. A file the server left out of its answer comes
  through its own download.

Core computes the key of a task only once the tasks it depends on have
finished, so it also restores keys the prefetch did not cover. Those restores
share their negotiate:

- A restore that needs a negotiate joins the open group of its mode: minimal
  for a restore of the result only, the session's mode otherwise.
- The group is sent once no restore has joined it for 20 ms, at the latest
  50 ms after its first restore, and at once when it holds 5,000 keys. A
  finished task releases the tasks that depend on it together, so their
  restores share one negotiate. A restore that arrives alone waits 20 ms
  more, less than one request to the cache server.
- The small files of the group's hits come in one `download-batch`, the way
  the prefetch fetches them.
- When the group's negotiate fails, each restore negotiates its own key. A
  group of one key, or a credential the server refused, gets no second
  negotiate: the restore is an `error`.

A restore uses the downloads of the negotiate answer. They do not last: the
cache server signs a URL for 15 minutes, or hands out a storage token that
lasts 20 to 60 minutes.

- A restore that starts 5 minutes or more after its negotiate, and needs a
  download, negotiates its key again first. A file needs no download when core
  already has it, when the prefetch or an earlier restore placed it, or when
  it is at most 256 KiB: small files come through `download-batch`.
- When the storage refuses a download with 400, 401, 403, 404 or 410, the
  restore negotiates its key again and downloads the files it still lacks.
- A restore negotiates again once at most. A download refused after that is
  an `error`. An entry the server no longer holds is a `miss`.
- A 5xx or a network error on a download is an `error`, with no new
  negotiate.
- A transfer's signature or storage token stays inside its origin (scheme,
  host and port). A redirect to another origin is followed without it.

### End of a run

Core gives `summary` 60 s. It gives `shutdown` 5 s to answer, then closes
stdin, waits 5 s and kills the process. The provider stays inside both.

| Moment | The provider waits | Entries not stored by then |
| --- | --- | --- |
| `summary` | Until the uploads end. It gives up after 20 s in which no upload moves forward, or after 50 s in all. | Not counted. The uploader goes on until `shutdown`. |
| `shutdown` | 4 s, then answers. | Dropped. |
| stdin closes without `shutdown` | For the uploader only, with the bounds of `summary`. | Dropped. |

An upload moves forward when a request is answered or when bytes of a blob
are read or sent. The 20 s is the bound the object cache's drain has. A drain
that gives up logs one line with the number of entries left. A signal that
kills the process still drops the queue.

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
