# ADR 0004 — An uploader capability on the provider session

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/registry` (`protocols/registry`),
  `credential-provider/v1`, capability `uploader-v1`
- **Amends**: [ADR 0003](0003-publication-ops-behind-a-negotiated-capability.md),
  Order step 4 and the consequence "the engine holds it and uploads"

## Context

Under [ADR 0003](0003-publication-ops-behind-a-negotiated-capability.md), the
engine asks the provider for a `publish` credential after `open` and uploads
every member in its own process. The engine therefore holds a publish bearer
during every upload, and it links a client for each registry protocol: npm, the
Go module proxy, OCI and `put-write/v1`.

The provider that opened the plan already holds the plan's write bearer. It
can upload the members itself, if it can read them. The engine packs each
member into a publication outbox: a private directory of the engine's user,
with a descriptor that names every file, its size and its digest. A provider
started by the same engine runs as that user and can read it.

## Decision

The provider uploads the members through one more op on the credential
provider session, defined by a capability both sides negotiate at
`initialize`.

### Names

The capability is `uploader-v1` (`CapabilityUploaderV1`). Its op is `upload`.
It uses the `credential-provider/v1` envelope, protocol version `1`, and the
bounds of the publication ops: 8 MiB and 12 levels.

The capability holds only beside `publication-v1`. The engine offers both, and
the provider echoes both. A capability list that names `uploader-v1` without
`publication-v1` is refused. In a session without the echo, both sides refuse
`upload` as unknown.

### The op

| Payload | Answer payload |
|---------|----------------|
| `planDigest`; `registry`, the registry URL the engine resolved for the member; `kind`, for a Put registry member only; `outbox`, the absolute path of the outbox directory; `member`, the member's outbox entry | `digest`: the artifact digest the registry stores. `platforms`: the digest of each `os/arch` artifact of a multi-platform member, absent otherwise. |

The request carries no credential, and the answer carries none. The provider
uploads with the bearer it holds for the opened plan.

The provider reads every file of the member from the outbox and checks it
against the size and digest the member names. A file that differs is refused
with `artifact_digest_mismatch`, so a file rewritten after the engine packed it
is not uploaded.

### Order

`upload` comes after `open` and before `release`. Before `open`, a provider
refuses it with `plan_not_open`. An upload whose `planDigest` is not the
opened plan's, or whose member the plan does not list, is refused with
`plan_mismatch`.

### Refusals

`plan_not_open`, `plan_mismatch`, `registry_not_served`,
`artifact_digest_mismatch`, `artifact_conflict` and `upload_failed`
(`UploadRefusalCodes`). `registry_not_served` means the provider does not
upload to the request's registry. `artifact_conflict` means the registry holds
another artifact at the member's version; nothing was uploaded.
`upload_failed` covers every other registry refusal and an upload that did not
complete.

## Rejected alternatives

- **The engine keeps the upload and the provider only answers the bearer.**
  This is ADR 0003. The engine holds a publish bearer and links every
  registry client.
- **The bytes travel in the request.** A line is at most 8 MiB, and a Go
  module zip may reach 500 MiB.
- **The engine passes open file descriptors.** The outbox already exists on
  disk, and each operating system passes descriptors its own way.
- **A separate uploader process.** The bearer would cross the engine, or the
  uploader would need a second grant from the issuer.

## Consequences

- Under `uploader-v1`, the engine asks for no `publish` credential. The rule
  that a provider refuses `credential` with purpose `publish` before `open`
  still holds for an engine that asks.
- No process outside the provider holds a publish bearer.
- The engine keeps every check it makes before an upload, and compares the
  answered digest with the digest it packed.
- A provider serves only the registries it holds a bearer for. Any other
  registry gets `registry_not_served`.
