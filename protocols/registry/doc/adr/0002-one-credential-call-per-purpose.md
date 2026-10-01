# ADR 0002 — One credential call per purpose, from the workspace's credential provider

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/registry` (`protocols/registry`),
  `credential-provider/v1`

## Context

The host-keyed seam ([ADR 0001](0001-host-keyed-credential-seam.md)) costs one
process and one cloud round trip per host, states no expiry, and lets the
engine choose where a bearer goes: the issuer never says which hosts it is good
for, so a redirect or a mirror entry can carry it elsewhere.

A hosted run adds a constraint: the engine must use the credentials the caller
allowed and nothing else. A source the engine discovers on its own (a user's
`~/.npmrc`, a cloud session on the machine) is outside that control.

## Decision

The engine asks the workspace's credential provider for one credential per
purpose, over a JSONL RPC, only when the process enables that purpose.

### Names

The reserved command is `credential-provider`, protocol version `1` (echoed in
every line), capability `credential-v1` (negotiated at `initialize`). The
purposes are `read` (downloads) and `publish` (uploads). The reserved
`publish-provider` spelling of ADR 0001 is never reused: an older CLI would
read the declaration as the marker it deleted. The conformance test pins both
names.

### Resolution

The provider is the one loaded extension that declares `credential-provider`,
found by the reserved-command resolution the cache provider uses. None means
absent: the engine behaves as without the feature. Two are an error
(`ErrProviderAmbiguous`) that fails the first request needing a credential and
names both extensions and the source of the choice; a command that needs no
credential still runs.

### Wire

One JSON object per line on the provider's stdin and stdout, strictly decoded:
unknown or duplicate members, `null`, trailing data and nesting deeper than
the protocol are refused.

```
→ {"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"capabilities":["credential-v1"],"runCredential":"…"}}
← {"protocolVersion":1,"id":1,"ok":true,"payload":{"protocolVersion":1,"capabilities":["credential-v1"]}}
→ {"protocolVersion":1,"id":2,"op":"credential","payload":{"purpose":"read"}}
← {"protocolVersion":1,"id":2,"ok":true,"payload":{"credential":{"bearer":"…","expiresAt":"2026-09-28T12:00:00Z","hosts":["put.putnami.dev"]}}}
→ {"protocolVersion":1,"id":3,"op":"shutdown"}
← {"protocolVersion":1,"id":3,"ok":true}
```

The request carries the purpose and nothing else. The provider decides the
hosts; the engine enforces them.

### Run credential

A hosted run authenticates as the run. The engine hands its opaque run
credential to the provider in the optional `initialize` member
`runCredential`: 1 to 16384 bytes of UTF-8 with no whitespace
(`ValidRunCredential`, `MaxRunCredentialBytes`, `RunCredentialPattern`). It
sends the member only when it holds a run credential. An engine that holds a
malformed value sends no `initialize` and fails the credential request, so a
hosted run never falls back to the provider's own credentials. Stdin is the
only channel: the environment, a file or an argument would expose it to every
repository process.

A provider that receives it MUST keep it in memory only: never in a file, an
environment variable, an argument, a child process's environment, or a log. It
should call `procguard.DenyInspection()` (`go.putnami.dev/sdk/extension/procguard`)
before it reads its first line. `CredentialInitializeParams` and
`CredentialRequest` format the value as `<redacted>`.

### Absence, refusal, failure

- **Absence** is `ok: true` with no `credential` member. The request goes on
  with its native credentials. A provider with no credential for the purpose,
  for example because no user signed in, answers absence.
- **Refusal** is `ok: false` with `error: {"code", "message"}`: a code matching
  `^[a-z][a-z0-9_]{0,63}$`, an optional message of at most 512 bytes. A
  provider refuses only when policy forbids the purpose. A refusal carries no
  hosts, so it fails every request of that purpose. The engine never falls
  back after a refusal: it is a decision, not a missing answer.
- **Failure**: a provider that cannot start, answers out of protocol or does
  not answer in time fails the work that needed the credential, with no
  fallback. A failure answers for 30 seconds; the next request after that asks
  again, restarting the provider at most once per 30 seconds.

### Host scoping

A credential names `hosts`: 1 to 64 lowercase entries, sorted and unique, each
a DNS name or IPv4 address with an optional port. The engine attaches the
bearer only to an `https` URL (or `http` to a loopback host) without userinfo
whose host and port match an entry; an entry without a port means the scheme's
default port. Any other request uses its native credentials.

### Expiry, sharing, lifecycle

- `expiresAt` is RFC 3339 UTC ending in `Z`. The engine keeps a credential
  until `expiresAt` minus the smaller of one minute and half the lifetime left
  on arrival, then asks again. An already-expired credential is an error.
  Absence and refusal hold for the rest of the process.
- The engine asks at most once per purpose while its answer is valid, across
  every host and request of the process; concurrent consumers share one
  exchange.
- The provider starts on the first credential a consumer needs. At process
  end the engine sends `shutdown` and closes stdin. The provider exits on
  stdin EOF, with or without `shutdown`, because an engine that dies leaves
  only the EOF.

### Enabling purposes

`--providers <list>` (global, comma list) enables purposes for one process:
`install` enables `read`, `publish` enables `publish`. `PUTNAMI_PROVIDERS`
supplies the list when the flag is absent; the flag wins. An unknown value is
a usage error naming its source. The feature is off by default. The CLI
reads `PUTNAMI_PROVIDERS` once and removes it from its environment, so neither
the provider nor any task, hook or extension command inherits it; a nested CLI
enables purposes only through its own command line. `upgrade`'s restart into the CLI it installed
continues the same invocation, so it sets `PUTNAMI_PROVIDERS`, never
`--providers`, which an older target refuses.

A portable execution request carries the list as `invocation.providers`, so the
executing engine enables exactly the caller's purposes. It also carries
`invocation.publication` (`protocols/runner`), which authorizes the plan's
publication tasks; the protocol refuses a publication task without it, and the
executing engine also refuses it over a plan with no registry or cloud effect.

### Consumers

The engine's own registry downloads (CLI archives, extension archives) use
`read`. Before a workspace's provider can start, a user-scope extension's
provider serves those downloads. On a hosted run the engine hands the `read`
credential only to the `workspace-fetch` job, over an inherited descriptor
([`tooling/cli` ADR 0055](../../../../tooling/cli/doc/adr/0055-run-credentials-stay-out-of-repository-processes.md)).
Extension processes of a local run and uploads keep `registry-token/v1`.

## Rejected alternatives

- **Host-keyed provider calls** (`credential {host}`). One call per host, and
  the engine chooses where a bearer goes; asking by purpose lets the issuer
  state the scope.
- **A JSON stdout on `registry-token/v1`.** Breaks every deployed producer and
  keeps one process per host.
- **Fall back to the host-keyed command after a refusal.** A hosted run would
  use a credential the caller did not allow.
- **On by default.** Without a serving producer it is a silent no-op that hides
  misconfiguration.
- **A new member without a negotiated capability** is normally refused by the
  versioning rule. `runCredential` is the one exception: `initialize` is the
  line that negotiates capabilities, so nothing could gate it.

## Consequences

- Two credential mechanisms coexist. With the flag off or no provider, every
  path is the ADR 0001 path. `registry-token/v1`, its `--materialize` form and
  the SDK's per-host memo retire once the cloud serves `credential-provider`
  and uploads move in process behind `publish`.
- The provider's answer is data the engine validates: this module ships a JSON
  Schema and a fixture corpus a second implementation is checked against.
- A bearer never reaches an output: `Credential` formats it as `<redacted>`,
  and the engine reports refusal codes and hosts, never bearers.
