# ADR 0057 — Publication authority stays in the engine

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/credentialprovider`,
  `internal/jobs`: release set, publication session, publication outbox,
  process capabilities; `internal/engine`; `internal/cli`)

## Context

A publication job runs repository code. When it holds a `publish` credential,
every process that code starts can read it, and the release-set provider learns
what the run published only from the final release request.

The registry protocol defines `publication-v1`: `resolve`, `open` and
`release` on the credential provider session, and a `publish` credential that
the provider scopes to the opened plan
([registry ADR 0003](../../../../protocols/registry/doc/adr/0003-publication-ops-behind-a-negotiated-capability.md)).
The extensions pack every managed member into an outbox and upload nothing
when `PUTNAMI_PUBLICATION_OUTBOX` is set
([extension SDK ADR 0004](../../../extension-sdk/doc/adr/0004-a-publication-job-packs-and-the-engine-uploads.md)).
This ADR decides what the engine does with both.

## Decision

### 1. The echo selects the path

The credential provider session offers `publication-v1` at `initialize` and
accepts an answer with or without it. When the provider echoes it, the session
is the run's only publication authority: the release-set plan resolves its
channels through the session, and no release-set provider process starts.
Without the echo, the release set publishes as it does without the capability.

### 2. One open, after the checks

The engine binds the plan tuple and the ancestry statement before planning
ends. The statement comes from the snapshot the process read at start, before
the first-use bootstrap, an install or the first hook ran repository code. A
missing, failed or shallow snapshot, or one bound to another commit, fails the
run before any job.

One `open` node runs per run. It waits for the bound request's barrier
commands, or, on a local run, for every task that neither publishes nor
depends on a task that publishes. It opens only when everything it waits for
succeeded and no task failed.

### 3. Jobs pack, the engine uploads

Each selected publication job receives a private outbox, mode `0700`, and none
of the variables that carry a registry or cloud credential, a credential
descriptor or a registry route. One upload node per publication job waits for
the job and for `open`. It reads the outbox, hashes every file again, and
refuses the outbox before any upload when a member is not the one the plan
assigns that job: another member, a member the plan does not select, another
version or another project. A member's registry is the registry its npm block
names, the `registries.go.origin` of its project, or the host of its OCI
repository. The node refuses an npm registry other than the one
`registries.npm.publish` names, compared after the managed npm rules normalize
both, and an OCI host other than the one `registries.oci.publish` names, as a
repository prefix or as a URL. It then asks the session for the `publish`
credential of each member's registry, applies the host rule, and uploads the
npm tarball, the Go module zip or the OCI layout in its own process. It writes
`published-image.json` for an OCI member and reports one `published-member`
event per upload. A job that wrote no descriptor packed nothing, so its node
uploads nothing. The upload nodes count as publication jobs for the release.

### 4. One release, final on an answer

The release carries the plan digest, the heads it moves from, the ancestry
stated at `open`, and the evidence. `open` and `release` are sent again once,
after the op timed out while the session was still live, and in no other
case; a session that ends fails the run. A refusal is a
bounded error that names its code; it moves no channel, and the run fails.

### 5. A bound request plans without the engine's nodes

The `open` and upload nodes run engine code only, and they exist only when
the provider echoes `publication-v1`, which a submitter does not know. A
bound request's expected plan names neither. The executing engine compares
that plan with its graph without them: an edge to an upload node becomes an
edge to the publication job it uploads for, and an edge to `open` is
dropped. The publication barrier check, the input admission and the
scheduler read the whole graph.

## Rejected alternatives

- **Hand the publication job a short-lived bearer.** Every process the job
  starts can read it while it is valid, and the provider still learns the
  artifacts only at release.
- **Keep the nested release-set provider process beside the session.** Two
  authorities could resolve different heads for one run.
- **Upload from a framework-owned child process.** It needs the bearer in a
  descriptor or its environment, which is the exposure this decision removes.
- **Trust the descriptor's identities.** A job packs what it wants; only the
  plan says what the run publishes.

## Consequences

- No repository process of a `publication-v1` run holds a `publish` bearer.
- A selected member outside npm, Go modules and OCI images fails the plan
  before `open`, because the engine has no upload for it.
- A bound request with `invocation.publication` publishes through
  `publication-v1` as a local run does, from the plan its submitter computed
  without the capability, and opens the plan digest a local run of the same
  commit opens. An expected plan that names an `open` or upload node is
  refused, whichever path the provider selects.
- A publication job whose result was reused instead of executed (a local or
  remote cache hit, or a coalesced result) packed nothing into this run's
  outbox, and a `published-member` event its result replays names an upload
  this run did not make. The release refuses such a run and names the job.
- The engine now carries the npm, Go module and OCI upload clients of the SDK.
- `.gen/version.json` keeps no image fields under the outbox: no reader in the
  same run needs them, and a same-session deploy takes its image from the
  released set.
