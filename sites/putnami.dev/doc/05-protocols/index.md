# Protocols

Putnami's three pillars — the **frameworks** (Go and TypeScript), the **tooling**
(CLI, extension SDK, CI), and the **platform** (Putnami Cloud and other deploy
targets) — do not integrate through shared code. They integrate through
protocols.

A protocol is a wire contract: a JSON shape with a published schema, a corpus of
valid and invalid fixtures, a strict parser in every language that implements
it, and at least one real consumer. Nothing in Putnami is allowed to become a
shared assumption. It becomes a protocol, or it does not cross a boundary.

This is the layer that turns the [principles](/docs/principles) from statements
into things a build can check.

---

## Why this layer exists

Most stacks integrate by convention. The CLI knows what the framework emits
because the same person wrote both. That holds until a second language, a second
consumer, or an agent arrives — and then the convention lives only in someone's
head, and drift is undetectable rather than merely unfixed.

Putnami's principles ask for guarantees a convention cannot carry:

| The principle | What the protocol layer does about it |
| --- | --- |
| **Deterministic and reviewable** | Intent is a JSON document in git, strict-parsed against a published schema, canonically serialized, and addressable by digest. Drift is a test failure, not a surprise. |
| **Observable by construction** | One versioned JSONL event envelope for every job, and one OTLP/JSON shape exported by every runtime — hand-rolled to the wire, proven byte-identical across languages. |
| **Automation is a first-class user** | Exit codes, `--output` modes, and the machine documents are a versioned contract, not an implementation detail that can move under an agent's feet. |
| **Security is foundational** | Reachability, identity claims, and signing-key lifecycles are declared and fail closed. An ambiguous route pattern fails the build rather than widening an allowlist. |
| **Data ownership is non-negotiable** | Database, storage, and transaction contracts are declaration-only. They describe what a workload needs and what it was given; they never own the data plane. |

The protocol layer is also what makes the polyglot claim honest. TypeScript and
Go do not "support the same features" — they are validated against the same
fixture corpus, and where they produce artifacts, those artifacts are pinned
byte-identical.

---

## From intent to infrastructure

The chain the rest of the documentation describes in pieces is, concretely, this:

<!-- cols: 30 30 40 -->

| You declare | The protocol that carries it | Who reads it |
| --- | --- | --- |
| `putnami.workspace.json`, `putnami.json`<br>*workspace and project identity* | [`workspace`](https://github.com/putnami/putnami/tree/main/protocols/workspace), [`extension`](https://github.com/putnami/putnami/tree/main/protocols/extension) | the CLI, every extension, every task |
| An HTTP handler<br>*the HTTP framework* | [`http-routes`](https://github.com/putnami/putnami/tree/main/protocols/http-routes) | your public edge, as a generated default-deny allowlist |
| A repository, a migration<br>*persistence* | [`database`](https://github.com/putnami/putnami/tree/main/protocols/database), [`migration`](https://github.com/putnami/putnami/tree/main/protocols/migration), [`transaction`](https://github.com/putnami/putnami/tree/main/protocols/transaction) | Go and TypeScript adapters, the deployer, the test provisioner |
| An event handler<br>*the events framework* | [`events`](https://github.com/putnami/putnami/tree/main/protocols/events) | clients, Event Servers, the managed Event Plane |
| An infrastructure need, next to the code that needs it<br>*infra requirements* | [`infra`](https://github.com/putnami/putnami/tree/main/protocols/infra) | the build aggregator, then one merged artifact per deploy target |
| A job<br>*the job runner* | [`job`](https://github.com/putnami/putnami/tree/main/protocols/job), [`runtime`](https://github.com/putnami/putnami/tree/main/protocols/runtime) | the CLI, the language SDKs, your CI |
| `putnami.ci.json`<br>*CI intent, reviewable in git* | [`ci`](https://github.com/putnami/putnami/tree/main/protocols/ci) | any execution plane — it reads the document, it never widens it |
| *Nothing. It is derived.*<br>*your project's own facts* | [`capabilities`](https://github.com/putnami/putnami/tree/main/protocols/capabilities), [`contracts`](https://github.com/putnami/putnami/tree/main/protocols/contracts), [`agentcontext`](https://github.com/putnami/putnami/tree/main/protocols/agentcontext) | your agent, through `putnami context pack` and MCP |

Read the last row again. Every other row is something a developer writes once,
next to the code it describes. The last row is what the system derives from all
of them — and it is the row an agent reads.

---

## The agent link

An agent does not need to be taught your repository. It reads the same contracts
your build reads.

**`putnami context pack`** aggregates one project's framework-owned facts into
an agent-context document: identity and dependency graph, composition roots,
capability / contract / infra / migration references, representative source
*ranges*, tests, adjacent docs, and provenance. It aggregates **by reference** —
paths, digests, and ranges, never file content — and runs a fail-closed
publish-safety gate before anything leaves the repository. The read-only MCP
`agent_context` tool serves it per request.

The rest of the operational surface is contracted the same way:

- `--output=json` / `--output=jsonl` and the exit-code taxonomy are the
  [`cli`](https://github.com/putnami/putnami/tree/main/protocols/cli) protocol,
  versioned and conformance-tested from both the Go and the TypeScript
  implementation.
- `--impacted` gives an agent the blast radius of a change before it acts.
- The [`runtime`](https://github.com/putnami/putnami/tree/main/protocols/runtime)
  event stream means the agent watching a job and the human reading the log are
  parsing the same lines.
- [`doctor`](https://github.com/putnami/putnami/tree/main/protocols/doctor)
  reports production-readiness gaps under a deployment profile, with a frozen
  check-code taxonomy and a baked remediation per check — so "what is wrong and
  what do I do about it" is data, not prose.

This is the difference between a project an agent can *edit* and a system an
agent can *operate*. The second one requires a contract.

---

## The protocol map

Every protocol below owns exactly one boundary. Its Go module name
(`go.putnami.dev/protocol/<name>`) deliberately stands apart from any single
consumer rather than belonging to whichever pillar defined it first.

### Workspace and intent

| Protocol | What it carries |
| --- | --- |
| [`workspace`](https://github.com/putnami/putnami/tree/main/protocols/workspace) | `putnami.workspace.json` and `putnami.json`: discovery, config layering, extension declarations, predictable resolution across global, workspace, and local scopes. |
| [`extension`](https://github.com/putnami/putnami/tree/main/protocols/extension) | `putnami.extension.json`: commands, tasks, pipelines, flags, and cache policy — declared, not coded. |
| [`template`](https://github.com/putnami/putnami/tree/main/protocols/template) | `putnami.template.json`: project templates stay portable and inspectable instead of hiding metadata in scripts. |
| [`ci`](https://github.com/putnami/putnami/tree/main/protocols/ci) | `putnami.ci.json`: triggers, branch rules, jobs, runners, publish/deploy clauses — plane-neutral, canonically normalized, digested, and rejecting literal credentials. |
| [`features`](https://github.com/putnami/putnami/tree/main/protocols/features) | Feature intent, repository evidence, durable specs, and the verification report that connects them. Maturity is an evidence ladder, not an adjective. |
| [`support`](https://github.com/putnami/putnami/tree/main/protocols/support) | The closed vocabulary behind `putnami.support.json` — one reviewed authority for what you may depend on. |
| [`architecture`](https://github.com/putnami/putnami/tree/main/protocols/architecture) | Architecture Rules as Code (ARC) and Domain Access & Replication Contracts (DARC). *Data may be copied. Authority may not be copied.* Opt-in, and free to change its wire format without a migration path. See [spec-driven development](/docs/concepts/spec-driven-development). |

### Execution, diagnostics, and observability

| Protocol | What it carries |
| --- | --- |
| [`cli`](https://github.com/putnami/putnami/tree/main/protocols/cli) | One exit-code taxonomy, one `--output` vocabulary, the reserved global-flag registry, and the machine documents (JSON, JSONL, MCP, session / plan / report files). |
| [`runtime`](https://github.com/putnami/putnami/tree/main/protocols/runtime) | The JSONL event envelope: logs, progress, phases, diagnostics, metrics, artifacts, results. |
| [`job`](https://github.com/putnami/putnami/tree/main/protocols/job) | The `--putnamiContext` document: workspace / project / extension / job identity, merged params with coercion rules, and the env-var mirror. |
| [`config`](https://github.com/putnami/putnami/tree/main/protocols/config) | Schema manifests, canonical field types, SHA-256 hashing, remote resolution, and dimension layering — identical in Go and TypeScript. |
| [`diagnostic`](https://github.com/putnami/putnami/tree/main/protocols/diagnostic) | One structured finding format, so no strict parser in the system invents its own error shape. |
| [`telemetry`](https://github.com/putnami/putnami/tree/main/protocols/telemetry) | OTLP/JSON metrics, traces, and logs — hand-rolled to the wire, byte-identical across languages, pinned by digest. |
| [`platform`](https://github.com/putnami/putnami/tree/main/protocols/platform) | `/healthz`, `/livez`, `/readyz`, `/version`, opt-in pprof: the same operational surface from every runtime, so operators never special-case a language. |

#### One bounded test story in every runtime

Go and TypeScript test jobs use the same runtime-event profile. Normal output is
one structured pass/fail/skip recap; the complete tool transcript travels as
debug detail and `--test-verbose` only promotes its visibility. Failure
diagnostics are capped at 16 records and 1,024 UTF-8 bytes per message. If the
cap is reached, both the final diagnostic and
`result.data.testSummary.failureDetailsTruncated` report the omitted count, while
the transcript is kept in the session's `events.jsonl`. Batched runs preserve
the same story per project rather than merging several suites into an
unattributed stream. Action-cache entries retain the structured result but not
transcript logs, so a warm cache hit cannot replay verbose detail;
`--test-verbose` shows the transcript only for a task that executes.

Go, TypeScript and Python test jobs also report each test case. The session
stream carries one `test:case` record per case, right before the task's
`task:end`: the full test name, its suite (Go package or test file), `passed`,
`failed` or `skipped`, the duration, and the file and line when the runner
knows them. Failed and skipped cases carry their output, cut to 4,096 bytes and
redacted like every other machine string. A task keeps at most 1,000 records
and 1 MiB of encoded cases, and a batch's members share 8 MiB, failed cases
first; `task:end` counts the rest in `testCasesDropped`.
Normal `--output=jsonl` keeps every case in `events.jsonl` only; `--verbose`
shows them live. A reader that does not know `test:case` skips the line, but a
validator built before this contract rejects it: upgrade the validator first.

### Data

| Protocol | What it carries |
| --- | --- |
| [`database`](https://github.com/putnami/putnami/tree/main/protocols/database) | Requirement manifests, bindings, and test bindings: logical datasources, exactly-one-transport connections, schema semantics, test provisioning policy. |
| [`transaction`](https://github.com/putnami/putnami/tree/main/protocols/transaction) | The unit-of-work descriptor and result envelope, with a closed outcome taxonomy and a retryable advisory that must agree with it. |
| [`migration`](https://github.com/putnami/putnami/tree/main/protocols/migration) | Startup, locking, state tracking, and rollback guarantees — plus bundle digest equivalence across runners. |
| [`storage`](https://github.com/putnami/putnami/tree/main/protocols/storage) | Object storage declared provider-neutrally: access levels, isolation scopes, signed-URL capability, lifecycle. Declaration only; the data plane stays out. |

### Reachability, events, and infrastructure

| Protocol | What it carries |
| --- | --- |
| [`http-routes`](https://github.com/putnami/putnami/tree/main/protocols/http-routes) | A provider-neutral reachability inventory with explicit public-edge visibility, provenance, canonical ordering, and a stable digest. Ambiguous patterns fail closed. |
| [`events`](https://github.com/putnami/putnami/tree/main/protocols/events) | Envelopes, stream frames, discovery, endpoint profiles, and a conformance runner that can be pointed at a deployed Event Server. |
| [`infra`](https://github.com/putnami/putnami/tree/main/protocols/infra) | Requirements committed next to the code, workload runtime intent, overrides, and the aggregated manifest with deterministic merge rules and provenance; its canonical bytes are a workload's deployment declaration. |

### Build, publish, and distribution

| Protocol | What it carries |
| --- | --- |
| [`cache`](https://github.com/putnami/putnami/tree/main/protocols/cache) | The remote build cache wire: negotiate, store/commit, batched writes, Action Cache and CAS shapes, capabilities, presigned transfers — plus the provider RPC. |
| [`oci`](https://github.com/putnami/putnami/tree/main/protocols/oci) | Optional registry fast paths layered on the OCI distribution spec, always degrading to the standard API elsewhere. |
| [`gomod`](https://github.com/putnami/putnami/tree/main/protocols/gomod) | Authenticated private Go module uploads with channel routing a plain VCS tag cannot express. |
| [`put`](https://github.com/putnami/putnami/tree/main/protocols/put) | Release archives, config members, migrations, site-content bundles and deployment declarations uploaded to the Put registry as immutable versions; only a release moves a channel. |
| [`registry`](https://github.com/putnami/putnami/tree/main/protocols/registry) | Publisher authentication without the framework owning a host list, a recipe model, or a stored credential. Absence of a credential is a supported answer. |
| [`sitecontent`](https://github.com/putnami/putnami/tree/main/protocols/sitecontent) | Content produced in one repository, mounted into a site in another as one content-addressed, self-verifying bundle. *This page's neighbours arrive that way.* |

### Security

| Protocol | What it carries |
| --- | --- |
| [`identity`](https://github.com/putnami/putnami/tree/main/protocols/identity) | The consumer-side identity vocabulary — well-known claims, principal kinds, the typed claims shape — authored once and generated into every language. |
| [`keyring`](https://github.com/putnami/putnami/tree/main/protocols/keyring) | The signing-key state machine with a legal-transition table, the private keyring and its public JWKS projection, and a versioned credential-digest grammar. Vocabulary and validation only; no cryptography. |

### Agents and assurance

| Protocol | What it carries |
| --- | --- |
| [`agentcontext`](https://github.com/putnami/putnami/tree/main/protocols/agentcontext) | The deterministic, redaction-safe per-project orientation document agents read instead of rescanning your repository. |
| [`capabilities`](https://github.com/putnami/putnami/tree/main/protocols/capabilities) | Everything one project contributes — config, schemas, discoverers, migrations, infra, health, lifecycle, versions — with provenance per entry. |
| [`contracts`](https://github.com/putnami/putnami/tree/main/protocols/contracts) | The canonical IR of a project's vocabulary, lowered by one compiler into Go and TypeScript types, JSON Schema, OpenAPI inputs, discovery metadata, and docs. |
| [`doctor`](https://github.com/putnami/putnami/tree/main/protocols/doctor) | Production-readiness findings under a deployment profile, a frozen check-code taxonomy, and waivers you commit deliberately rather than forget silently. |
| [`doccov`](https://github.com/putnami/putnami/tree/main/protocols/doccov) | No wire shape of its own: a ratchet that fails the build when too many wire fields cannot be explained from the types and schemas alone. |

The reviewed support status of every protocol is published on
[Support status](/docs/support), which is generated from the workspace catalog.
This page deliberately does not restate it — a status has exactly one home.

---

## What earns the name

A package in `protocols/` is not a protocol because of where it lives. It meets
a bar:

1. **An index entry** — it is declared in the map, not discovered by accident.
2. **A JSON schema** for every wire shape.
3. **A fixture corpus** with `valid/` and `invalid/` cases. This is the
   cross-language test surface.
4. **Strict parsing and a conformance test** that runs the corpus through the
   parser and the validator.
5. **At least one real consumer**, or an explicit *defined, not yet adopted*
   marker. **A protocol without consumers is a spec, not a contract.**
6. **A versioning guard** when the contract is versioned: the protocol version
   is pinned by test, and committed artifacts enforce an acceptance window.
7. **Cross-language equivalence fixtures** when more than one language
   implements it.

Point 5 is the one that keeps this directory honest. Point 3 is the one that
makes "polyglot" mean something: the fixtures are shared, so a Go implementation
and a TypeScript implementation cannot quietly disagree.

---

## Conformance is the proof

The repository publishes a
[conformance matrix](https://github.com/putnami/putnami/blob/main/protocols/README.md#conformance-matrix)
— who implements or consumes each protocol today, across the CLI, the extension
SDK, the Go framework, the TypeScript framework, and the platform. It
distinguishes three states:

- **conformant** — tested against the protocol package or its fixtures;
- **aligned by hand** — mirrored deliberately, drift possible, and marked as
  such;
- **not a consumer**.

That middle state is published on purpose. A matrix that only showed green
would be marketing; the value of this one is that it names exactly where the
guarantee is a test and where it is still a promise.

Where two languages both produce an artifact, equivalence is pinned rather than
assumed: config hashes, migration bundle digests, infra manifests, capability
and contract serializations, and the telemetry wire are all compared
byte-for-byte against shared goldens.

---

## Where protocols land: dev, cloud, intelligence

The same contracts carry across all three Putnami surfaces. That is the point of
having them.

**putnami.dev** — the frameworks and the tooling — is where intent is declared
and derived. Protocols make that declaration precise: what a project is, what it
exposes, what it needs, and what it emits.

**Putnami Cloud** adds operational depth on top of the same documents: release
provenance, runtime correlation, incident context, rollout protection. It reads
`http-routes` to build an allowlist, `infra` to provision, `ci` to execute,
`telemetry` to observe. It does not get a private dialect.

**Putnami Intelligence** makes the workspace queryable: agent orientation over
MCP, a versioned workspace index, and spec-driven development — features, specs
and architecture recorded next to the code. The review-and-audit loop — findings
over versioned evidence, with freshness and receipts — is where this is going.
It is a standalone product: it delivers value with neither Putnami frameworks
nor Putnami Cloud, through provider adapters that normalize external systems
into the same evidence model. Adopting putnami.dev improves its structural
precision; adopting Putnami Cloud improves its operational depth.

And Cloud is held to the same rule as anyone else: it integrates through the
same published contracts available to third-party adapters. Its advantage is
zero-configuration correlation across workspace, environment, revision,
deployment, and telemetry — not a private semantic backchannel.

---

## Read next

- [Concepts](/docs/concepts) — the layers these contracts sit between.
- [Principles](/docs/principles) — the constraints the protocols enforce.
- [Support status](/docs/support) — what you may depend on today.
- [Provision infra and run conformance packs](/docs/how-to/provision-infra-and-run-conformance-packs) — a protocol, used.
- [`protocols/` in the repository](https://github.com/putnami/putnami/tree/main/protocols) — the schemas, the fixtures, and the conformance matrix.
