# ADR 0056 — A publish dry run asks each registry

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`MemberProbe`),
  `go.putnami.dev/sdk/extension` (`memberprobe`, `dockerpublish`),
  `@putnami/go` (`publish-go`), `@putnami/typescript` (`publish-npm`),
  `@putnami/cli` (`internal/jobs` member-probe report, `internal/engine`)

## Context

A registry refuses to overwrite a published version. `putnami publish
--dry-run` executes every publish job in simulation: it resolves the version,
checks the staged artifact, and stops before the upload. A dry run that asks no
registry passes on a release whose real publish fails part-way: the members
before the conflict are written, the one the registry already holds is refused,
and the release set never commits.

Whether a version exists is a question only its registry answers, and each
registry answers in its own protocol. Four kinds publish from a workspace: npm
packages, Go modules, OCI images and put archives.

## Decision

1. **Each publisher asks its own registry; the CLI aggregates.** A publish job
   that runs under `--dry-run` asks the registry it would write to, once per
   member, and reports the answer. The CLI reads every answer once the run
   ends. It knows no registry protocol.
2. **One record carries the answer.** A publisher emits one `artifact` runtime
   event of kind `member-probe` per member, whose payload is the
   `MemberProbe` record of `protocols/extension`:

   | Field | Meaning |
   | --- | --- |
   | `ecosystem`, `coordinate`, `version` | The member the real publish would write. |
   | `platform` | Optional `os/arch`, for a member published as one artifact per platform. |
   | `registry` | The endpoint asked, without credential, query or fragment. |
   | `state` | `absent`, `identical`, `conflict` or `unverified`. The set is closed. |
   | `artifactDigest` | Optional sha256 digest of the local artifact. |
   | `registryDigest` | Optional sha256 digest the registry serves. |
   | `reason` | Required for `conflict` and `unverified`. Bounded and free of credentials. |
   | `anonymous` | Optional. True when the request carried no credential. |

   The states mean:

   - `absent`: the registry does not hold the version. The publish uploads it.
   - `identical`: the registry holds the version with the digest of the local
     artifact. The publish reuses it.
   - `conflict`: the registry holds the version with another digest, or the dry
     run has no local artifact to compare with.
   - `unverified`: the registry gave no usable answer: a network error, a
     timeout, a 401 or 403, a 5xx, or an answer outside the protocol.

   The parser is strict: an unknown field or state is an error. A
   `member-probe` event is never publication evidence, and a dry-run event
   never becomes a `published-member` event.
3. **A probe only reads.** It sends GET or HEAD, through the HTTP policy of its
   publisher's real publish. The managed npm, Go and put probes ignore the
   ambient proxy, follow no redirect, bound the body they read, and redact the
   excerpt they quote. The OCI probe uses the registry client of the image
   push, and the unmanaged npm probe asks through `npm` (see Known limits). A
   probe sends a credential only when the publisher's existing credential
   sources yield one, and never fails because none did. It requests no
   one-time password and no publish lease.
4. **The publish job still succeeds.** A job returns `OK` whatever its probe
   says. The CLI fails the run, so one dry run reports every conflict of the
   release instead of stopping at the first.
5. **The CLI prints one report and fails once.** After an executing dry-run
   publish, with or without a release-set plan, the CLI:
   - prints each `conflict` and `unverified` probe with its member, registry,
     version and reason, each `identical` member as reused, and a count of
     `absent` members;
   - fails the run when any probe is `conflict` or `unverified`, with one
     message that names every such member;
   - fails the run on an event the strict parser refuses, and on a probe whose
     version differs from the version the plan publishes for that member.
6. **A member no publisher probed is a warning.** With a plan, each selected
   member without a probe is printed as not probed, with its publisher and its
   publish step. The run does not fail on it, and never omits it. Without a
   plan, a run whose jobs emitted no probe prints one warning that nothing
   asked.
7. **An anonymous `absent` is a weaker answer, and the report says so.** A
   registry answers a request without a credential for a private member as it
   answers for a missing one. When any `absent` verdict was reached without a
   credential, the report prints one warning that says to sign in and run the
   dry run again.

### How each kind asks

| Kind | Request | Comparison |
| --- | --- | --- |
| npm, release-set member | One GET of the version's tarball. No `npm` process. | sha256 of the served tarball against the tarball `bun pm pack` writes, packed only when the registry holds the version. |
| npm, any other publication | `npm view <name>@<version> dist --json`, with the environment of the real publish. | The `dist.integrity` npm reports against the tarball `npm pack --ignore-scripts` writes. |
| Go module | One GET of the proxy zip. | sha256 of the served zip against the staged zip. A dry run under a plan stages no zip, so a held version is a `conflict`. |
| OCI image | One manifest HEAD of the version tag, or of the digest for an image project. | The manifest digest against the packaged one. With no packaged image, the plan supplies the coordinate and version, and a held version is a `conflict`. |
| put archive | One HEAD of the download endpoint, once per platform. | The digest the registry advertises against the local archive. |

The put probe is `memberprobe.ProbeArchive` in the extension SDK. The publisher
of put archives lives outside this repository and calls it from its own
dry-run branch.

## Known limits

- **Unmanaged npm cannot say whether it was anonymous.** npm's configuration
  owns the registry and the credential, so the probe asks through `npm`. It
  cannot observe whether npm sent a credential and never sets `anonymous`. A
  host without `npm` reports `unverified`.
- **Unmanaged npm is stricter than its real publish.** The real publish of a
  package outside a release set reuses any version the registry already holds,
  without comparing content. The dry run reports a `conflict` when the content
  differs.
- **A hosted run probes without a credential.** In a job of a hosted run, the
  credential seam starts no process and returns no token
  ([ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)). Its
  dry run reads public registries without a credential, and reports
  `unverified` for a registry that requires one to read.
- **A probe is a statement about one instant.** Another publisher can write the
  version between the dry run and the publish. The registry's refusal at
  publish time stays the authority.

## Consequences

- `publish --dry-run` needs network access to each target registry. Without
  it, every probe is `unverified` and the run fails with the reason. A plan-only
  preview (`--plan`) executes no job and asks nothing.
- Publish tasks declare `cache: false`, so a verdict is never restored from a
  cache.
- A publisher written against an older SDK emits no probe. Under a plan, the
  report names each of its members as not probed.
- The report is written to standard error; `--quiet` keeps the failures and the
  warnings and drops the reused and absent lines.

## Rejected alternatives

- **The CLI asks every registry itself.** It would duplicate each publisher's
  route, credential source and HTTP policy in the core, and a registry kind an
  external extension owns could not be probed at all.
- **The publish job fails on a conflict.** A failed job skips every job that
  depends on it, so a conflict in one member would hide the probes of the
  members published after it.
- **A missing probe fails the run.** A publisher that predates the contract
  would turn every dry run red without stating a conflict. The warning names
  the member and the step to fix instead.
- **Treat an unreachable registry as absent.** The dry run would pass without
  having asked, which is the failure this decision removes.
