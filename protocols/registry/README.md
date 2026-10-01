# protocol/registry

The registry credential seams between the framework and the credential issuer:

- `credential-provider/v1`: the engine asks the workspace's credential provider
  for one credential per purpose, over a JSONL RPC, when the process enables
  it (`--providers`). See [ADR 0002](doc/adr/0002-one-credential-call-per-purpose.md).
- `registry-token/v1`: publishers and installs run
  `putnami cloud registry-token --host <host>` and read a bare bearer. See
  [ADR 0001](doc/adr/0001-host-keyed-credential-seam.md).

## Why

A publisher has to authenticate to a package, image, or module registry. The
obvious implementation — teach the framework a list of Putnami hosts, a recipe
model, and a credential file — makes the core untestable without a cloud and
useless with a different one.

This protocol keeps the framework cloud-agnostic by passing the host as **data**
to a command the cloud owns. The framework therefore stores no host list, no
recipe model and no credential file, and every failure mode collapses into the
same answer: *no token*, fall back to the standard publish floor.

## What: `credential-provider/v1`

The provider is the one loaded extension that declares the reserved command
`credential-provider`. The engine starts it on the first credential a consumer
needs, speaks one JSON object per line on its stdin and stdout, and stops it when
the process ends:

```
→ {"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"capabilities":["credential-v1"]}}
← {"protocolVersion":1,"id":1,"ok":true,"payload":{"protocolVersion":1,"capabilities":["credential-v1"]}}
→ {"protocolVersion":1,"id":2,"op":"credential","payload":{"purpose":"read"}}
← {"protocolVersion":1,"id":2,"ok":true,"payload":{"credential":{"bearer":"…","expiresAt":"2026-09-28T12:00:00Z","hosts":["put.putnami.dev"]}}}
→ {"protocolVersion":1,"id":3,"op":"shutdown"}
← {"protocolVersion":1,"id":3,"ok":true}
```

| Rule | Meaning |
|------|---------|
| Purpose | `read` (downloads) or `publish` (uploads). `--providers install` enables `read`; `--providers publish` enables `publish`. A purpose the process did not enable is never asked for. |
| Absence | `ok: true` without a `credential` member. The request uses its native credentials, as without a provider. A provider that holds no credential for the purpose answers absence. |
| Refusal | `ok: false` with `error.code` (`^[a-z][a-z0-9_]{0,63}$`) and an optional `error.message` (at most 512 bytes). A provider refuses only when policy forbids the purpose. A refusal carries no hosts, so it fails every request of that purpose, whatever its host, with the code; nothing falls back to another credential. |
| Hosts | 1 to 64 lowercase entries, sorted and unique: a DNS name or IPv4 address with an optional port. The bearer goes only to an `https` URL (or `http` to loopback) without userinfo whose host and port match an entry; an entry without a port means the scheme's default port. |
| Expiry | `expiresAt` is RFC 3339 in UTC, ending in `Z`. The engine renews the credential before it expires, by the smaller of one minute and half the lifetime it had on arrival. |
| One call per purpose | The engine asks at most once per purpose while its answer is valid, across every host and request of the process. Absence and refusal hold for the rest of the process. |
| Failure | A provider that cannot start, crashes, answers out of protocol or does not answer in time fails the request, with no fallback. The failure answers for 30 seconds; the next request after that asks again and starts an exited provider, or one that did not answer in time, again, at most once per 30 seconds. |
| Lifecycle | The engine ends the session with `shutdown` and closes the provider's stdin. The provider exits when its stdin reaches EOF, whether or not it received `shutdown`. |
| Strictness | Unknown or duplicate members, member names that differ only in case, `null`, trailing data, and nesting deeper than the protocol are refused; so is an answer to a request that was never sent, which ends the session. |
| Run credential | On a hosted run, `initialize` also carries `runCredential`, the run's opaque bearer: 1 to 16384 bytes of UTF-8 with no whitespace. Without one, the member is absent and the line is the one above. The provider keeps the value in memory only; [ADR 0002](doc/adr/0002-one-credential-call-per-purpose.md) states the rules. |

Two declaring extensions are an error (`ErrProviderAmbiguous` in the CLI) that
fails the first request needing a credential; no declaring extension means the
feature is absent. The choice holds for one process: the CLI removes
`PUTNAMI_PROVIDERS` from its environment, so neither the provider nor anything
else it starts inherits it. `upgrade`'s restart into the CLI it just installed
continues the same invocation and sets the variable to the enabled list.

| Symbol | Meaning |
|--------|---------|
| `CredentialProviderCommand` | The reserved command, `credential-provider` |
| `CredentialProtocolVersion` / `CapabilityCredentialV1` | The RPC version (`1`) and the capability negotiated at `initialize` |
| `PurposeRead` / `PurposePublish` / `ValidPurpose` | The two purposes |
| `CredentialRequest` / `CredentialResponse` / `CredentialRefusal` | The line envelopes and the refusal |
| `CredentialInitializeParams` / `CredentialInitializeResult` / `CredentialParams` / `CredentialResult` / `Credential` | The op payloads |
| `ParseCredentialRequest` / `ParseCredentialResponse` / `ParseCredential*` | Strict decoders with validation |
| `ValidateCredential` / `ValidateRefusal` / `ValidCredentialHost` / `ValidCredentialBearer` | The validators |
| `Credential.Serves` | The host-scoping rule every engine applies before it attaches a bearer |
| `ValidRunCredential` | The run-credential rule: `ValidBearer`, valid UTF-8, at most `MaxRunCredentialBytes` |
| `Max*` bounds and `*Pattern` grammars | Line, host, bearer, refusal, capability and run-credential limits, shared with the schema |

`Credential` formats its bearer as `<redacted>` under `%v`, `%+v` and `%#v`, so a
logged value never carries it. `CredentialInitializeParams` does the same for
its run credential, and `CredentialRequest` prints its payload's size, never its
bytes.

## What: `registry-token/v1`

This seam has no JSON wire. The contract is a CLI invocation and the exact
meaning of its stdout:

```
putnami cloud registry-token --host <host>
  → stdout: a bare bearer token, nothing else        (exit 0)
  → exit non-zero / empty stdout: no credential       (fall back to explicit /
    native credentials; the cloud's stderr is surfaced as a hint)
```

For this seam the module owns five things:

| Symbol | Meaning |
|--------|---------|
| `CLIExecutableEnv` | `PUTNAMI_CLI_EXECUTABLE`, the absolute path of the CLI process that spawned an extension |
| `SeamParentCommand` / `SeamSubcommand` / `SeamHostFlag` | The exact invocation tokens (`cloud`, `registry-token`, `host`) both repositories spell the same way |
| `ValidBearer` | The bare-bearer rule: non-empty, no internal whitespace |
| `PublishProviderCommandName` | The retired capability marker, kept as a reserved spelling (see below) |
| `ProtocolVersion` | The seam version, pinned at `1` |

The CLI also prefixes that executable's directory to the extension process
`PATH`. `CLIExecutableEnv` is the authoritative contract for new SDK consumers;
the `PATH` prefix keeps existing extensions that still launch the bare
`putnami` name working with a workspace-local or bootstrapped CLI.

`ValidBearer` is the whole reason the seam is safe to consume: a value carrying
whitespace is a human status line written to the wrong stream. Sent verbatim it
becomes a malformed `Authorization: Bearer` header that the registry rejects as
an opaque 401, so the seam treats it as "no token" instead.

## Producers and consumers

**`credential-provider/v1` producer** — none ships yet. `@putnami/cloud` is the
intended producer, in a separate repository. Until it declares the command, the
flag enables nothing and every path below stays on `registry-token/v1`.

**`credential-provider/v1` consumer** — the CLI engine
(`tooling/cli/internal/credentialprovider`). Its first consumer is the CLI's own
archive download, `AuthorizeRegistryRequest` in
`tooling/cli/internal/extension`, which uses the `read` purpose. A host outside
the credential's `hosts`, an absent provider, or `--providers` off keeps the
request on the host-keyed seam, unchanged.

**`registry-token/v1` producer** — `putnami cloud registry-token`, implemented by `@putnami/cloud`
in a separate repository. This repository ships no producer, which is exactly
why the seam is host-keyed: a core-only install simply has no such command, and
that is a supported state rather than an error.

**`registry-token/v1` consumers** — all reach the seam through the extension SDK client
`go.putnami.dev/sdk/extension/registrycred` (`tooling/extension-sdk/registrycred`),
which owns the 30-second bound and the empty-token-on-failure rule:

An extension spawned by the CLI invokes the absolute executable carried in
`CLIExecutableEnv` without relaunching through a workspace version pin, so the
exact local bootstrap binary works without a global install.
Direct SDK use without that contract retains `putnami` on `PATH` as its
cloudless-compatible fallback.

| Consumer | Publishing path |
|----------|-----------------|
| `go/extension/internal/jobs/publish/gomodule.go` | Go module upload (`Authorization: Bearer` on the [`gomod`](../gomod/README.md) write endpoints) |
| `typescript/extension/cmd/putnami-ts/publish.go` | npm registry publish |
| `tooling/extension-sdk/imagepkg` | Read-only credential compatibility for a literal external base-image host; local project bases and output registries never use it |
| `tooling/extension-sdk/dockerpublish` | Docker registry login and push; managed OCI publication requests an exact workspace/package/action lease |

Every one of them degrades to explicit or native credentials (`.npmrc`, the
platform keychain, an explicit token) when the seam yields nothing.

## The retired publish-provider marker

`PublishProviderCommandName` (`publish-provider`) was a marker command an
extension declared so the framework could feature-detect a publish-capable cloud
at extension-load time. That gate is deleted on both sides: nothing declares the
command and nothing looks for it. Advanced publishing is selected by the tasks a
manifest declares; credentials come from this seam, which is host-keyed and
needs no capability probe.

The name survives as a **reserved spelling**. Both repositories assert its
absence against this constant, so a reintroduced marker fails a conformance test
instead of quietly becoming a second capability mechanism. The reasoning is in
[ADR 0001](doc/adr/0001-host-keyed-credential-seam.md).

The credential provider does not reuse it: an old CLI would read a new
declaration of `publish-provider` as the marker it deleted. The new command is
`credential-provider`, and `TestConformance_CredentialProvider` asserts the two
names differ ([ADR 0002](doc/adr/0002-one-credential-call-per-purpose.md)).

## Versioning and compatibility

`ProtocolVersion` and `CredentialProtocolVersion` are `1` and are pinned by
[`conformance_test.go`](conformance_test.go), so a bump is a reviewed act with a
migration story rather than a silent edit.

`credential-provider/v1` is closed: a new member, op, purpose or refusal field is
a new capability negotiated at `initialize`, never a silent addition, because
both sides decode strictly. The one exception is `initialize`'s optional
`runCredential`, added before any producer shipped; the amendment to
[ADR 0002](doc/adr/0002-one-credential-call-per-purpose.md) records why.

`registry-token/v1` retires only after the cloud producer serves
`credential-provider`: the SDK's `--materialize` form and its per-host,
per-process credential memo retire with it. Two follow-up changes move the
remaining consumers: one hands credentials to extension processes without the
environment, and the other moves uploads in process behind the `publish`
purpose.

Compatibility of `registry-token/v1` is cross-repository and therefore conservative:

- The three invocation tokens are **frozen**. Renaming any of them breaks every
  published framework build against every deployed cloud at once, in both
  directions, so a rename is a new protocol version and not a patch.
- The stdout contract is **additive-hostile**: stdout carries the bearer and
  nothing else. Adding a second line, a JSON envelope, or a status banner is a
  breaking change even though no field name moves, because `ValidBearer` is what
  consumers gate on.
- Absence is **not** a failure. A missing cloud, an unmanaged host, and a
  signed-out user are all the same supported outcome, so a cloud may stop
  managing a host without any framework change.

## Schemas and fixtures

`credential-provider/v1`:

- [`schemas/credential-provider-v1.json`](schemas/credential-provider-v1.json) —
  one JSON Schema (draft-07) for every request and response line, with the same
  bounds and grammars as the Go constants.
- [`fixtures/credential-provider/valid/`](fixtures/credential-provider/valid) —
  every op's request and response, absence, refusal with and without a message,
  a fractional-second expiry, and an `initialize` with a run credential.
- [`fixtures/credential-provider/invalid/`](fixtures/credential-provider/invalid) —
  unknown, duplicate, case-variant and `null` members, trailing data, a wrong version, an
  unknown op or purpose, bearers that are empty or carry whitespace, hosts with a
  scheme, path, userinfo, wildcard, uppercase letter or out-of-range port,
  unsorted or duplicate hosts, a non-UTC or missing expiry, malformed
  refusals, and run credentials that are empty, carry whitespace, exceed
  16384 bytes or are spelled `RunCredential`.

A fixture is one line. A response fixture names the op it answers:
`response-<op>-<case>.json`. `TestCredentialFixtures` requires every valid
fixture to parse and re-encode byte for byte and every invalid fixture to be
refused. `TestCredentialSchemaTracksWireShape` fails when a Go wire field and
the schema disagree on a member, a required member, a closed object, an enum,
a pattern or a bound. `TestCredentialFixturesAgainstTheSchema` evaluates every
fixture against the schema: a valid fixture must pass it, and an invalid one
must fail it unless the schema's description names the rule it breaks as one
a schema cannot express.

`registry-token/v1` carries no JSON document, so it has neither a schema nor a
fixture corpus. Its behavioural corpus is
[`conformance_test.go`](conformance_test.go): it pins `ProtocolVersion`, the
three invocation tokens, the reserved marker name, and a table of accepted and
rejected bearer spellings (plain token, JWT-with-space, empty, leading space,
trailing newline, internal tab, human status line).

## Support status, owner, and evidence

| | |
|---|---|
| **Status** | `preview` — see the `go.putnami.dev/protocol/registry` entry in the workspace-root [`putnami.support.json`](../../putnami.support.json), which is the only authority for this value |
| **Owner** | The `protocols` scope (`protocols/putnami.json`). This module is the sole authority for the credential seam; no publisher may re-spell the invocation or re-define what a valid bearer is. |

Evidence behind `preview`:

- `ProtocolVersion`, the invocation tokens and the bearer rule are pinned by a
  conformance test, so the contract cannot drift unreviewed;
- four independent real consumers use `registry-token/v1` in this repository
  (table above), each with a tested fallback when the seam yields nothing;
- `credential-provider/v1` has a JSON Schema, a valid/invalid fixture corpus
  that a second implementation can be validated against, and a CLI consumer
  tested against an out-of-process fake provider.

Evidence still missing for `stable`:

- the producers live in another repository, so no test here can exercise
  either seam end to end against the real issuer;
- no producer serves `credential-provider/v1` yet.

Per the [support protocol](../support/README.md), `preview` means public and
usable, but subject to change before it becomes stable. It is not a statement
about reliability of the consumers, which fall back safely by construction.

## Product feature and durable decisions

**No user-facing feature is declared for this module, deliberately.** A feature
in [`protocols/features`](../features/README.md) is authored product intent with
an owner and a maturity target; this module is a machine-to-machine boundary
with no user-visible surface of its own. The outcome a user actually has —
"publishing works against a private registry without me configuring
credentials" — is owned by the publishing commands of `@putnami/cli` and the
language extensions, and declaring a second feature here would mint an
artificial product identity for a technical seam. Because a spec details exactly
one already-authored feature and can never mint one, this module has no spec
either.

Durable decisions:

- [ADR 0001 — Registry credentials come from a host-keyed cloud command](doc/adr/0001-host-keyed-credential-seam.md)
- [ADR 0002 — One credential call per purpose, from the workspace's credential provider](doc/adr/0002-one-credential-call-per-purpose.md)
