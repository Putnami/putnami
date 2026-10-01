# protocol/oci

Putnami OCI registry extensions that layer on the standard OCI distribution spec.
Image content is pushed with any standard registry client; these contracts are
the **optional** fast paths a Putnami-managed registry advertises and the
framework uses when present, always degrading to the standard distribution API
elsewhere (gcr, ghcr, Docker Hub, …).

## tag-digest/v1

Assign every release ref to a digest in one authenticated, digest-addressed call
instead of one `GET`+`PUT` manifest round trip per tag.

```
GET  /v2/_putnami/capabilities
     → 200 {"apis": ["tag-digest/v1", ...]}   ; anything else → no fast path
POST /v2/_putnami/tag-digest
     {"repository": "team/app", "digest": "sha256:…", "tags": ["0.0.0-abc1234", "latest"]}
     → 2xx                                     ; all tags applied to the digest atomically
```

Capability discovery mirrors the remote-cache protocol: probe, take the fast path
when offered, fall back otherwise. The `_putnami` path segment cannot collide
with a repository name (distribution names may not start with `_`), the same
reservation trick as `/v2/_catalog`.

Why an extension exists at all, and why it must always degrade, is recorded in
[`doc/adr/0001-optional-registry-fast-paths.md`](doc/adr/0001-optional-registry-fast-paths.md).

## Producers and consumers

| Role | Implementation |
| --- | --- |
| Client (in repo) | `tooling/extension-sdk/oci` — `push.go` probes the capabilities endpoint, takes `tag-digest/v1` when advertised, and logs and falls back to per-tag manifest `PUT`s on any failure. |
| Server (out of repo) | The Putnami-managed OCI registry, implemented in the cloud repository. No public server implementation exists in this repository. |
| Non-participants | Any standard registry (gcr, ghcr, Docker Hub). They never answer the probe, so clients use the standard distribution API unchanged. |

The client never depends on the fast path existing: a missing endpoint, an
unknown capability list, an auth failure, or a non-2xx response all mean "use
the standard distribution API".

## Versioning and compatibility

`ProtocolVersion` (pinned by `conformance_test.go`) is bumped whenever a
wire-visible change would break an existing client or server, so each side can
reject a mismatch instead of misreading a payload. Individual extensions carry
their own version inside the capability string (`tag-digest/v1`): a breaking
change to one extension is advertised as a new capability, not a mutation of the
old one. Unknown entries in `apis` are ignored, so a registry may advertise
capabilities a client has never heard of. Because every extension is optional,
a client that understands none of them stays fully functional.

## Conformance

`strict.go` strict-decodes and validates each message;
[`fixtures/<message>/{valid,invalid}`](fixtures/) is the shared corpus every
implementation must agree on. There is no JSON Schema for these messages: the
Go types plus the fixture corpus are the normative surface, and the request
bodies are small enough that a second description would be the thing that
drifts.

## Ownership, support, and evidence

**Owner** — the `protocols` standardization layer. `protocols/oci`
(`go.putnami.dev/protocol/oci`) is the single owning project for these
extensions; the standard OCI distribution spec it layers on is owned upstream
and is never redefined here.

**Support status** — `preview` in the workspace-root
[`putnami.support.json`](../../putnami.support.json) catalog, whose vocabulary
[`protocols/support`](../support/README.md) defines. Public and usable, but the
shape may still change before it carries a stable commitment.

**Evidence for that status**

- Real client adoption: `tooling/extension-sdk/oci` uses the fast path in the
  publish pipeline and is tested against both the fast path and the fallback
  (`push_registry_test.go`, `tagdigest_test.go`).
- Strict parsing plus a valid/invalid corpus for both messages, with
  `ProtocolVersion` pinned by `conformance_test.go`.
- What is *missing* for `stable`, and why the status is not higher: the only
  server implementation lives outside this repository, so no cross-implementation
  conformance run exists here; there is no published JSON Schema; and there is
  no second independent client. Promotion needs at least a publicly verifiable
  server-side conformance run against this corpus.
- Nothing regresses while it is `preview`: because the extension is optional and
  probe-gated, a client on an older or newer contract still publishes correctly
  through the standard distribution API.

**Owning user feature** — none, deliberately. These extensions are an
optimization of an operation a standard registry already performs; the
user-visible outcome (a release's refs point at the pushed image) is identical
with or without them, so a product feature would describe latency, not a
capability. The durable decision lives in [`doc/adr/`](doc/adr/) instead of a
spec, which by contract details exactly one authored feature.
