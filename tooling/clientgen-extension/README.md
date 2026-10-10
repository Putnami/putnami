# @putnami/clientgen

`@putnami/clientgen` turns the contract emitted by a Putnami provider into
strict Go and TypeScript clients, then verifies their adoption across a
workspace. The provider declaration is the authority for schemas, typed errors,
security, service identity, idempotency, resilience, stream direction and the
REST/JSON, Connect/protobuf, SSE and WebSocket transports it exposes.

## Provider to consumer

Declare the provider API and enable its native client generator. In TypeScript,
add `clientGenerator()` to the application; in Go, add
`api.Clients(...).From(apiPlugin)`. The provider build writes
`.gen/clientgen/config.json` and its marked OpenAPI and Proto artifacts.
First-party mode is the default and rejects any contract semantic that the
shared client IR cannot represent.

Generate every configured target:

```bash
putnami clientgen --projects my-provider
```

Each generated package exports typed clients and registration functions. Import
the actual symbols recorded in its `client.putnami.json` manifest and register
the binding on the consumer module:

```typescript
import { module } from '@putnami/application';
import { registerItemsClient, ItemsClient } from '@acme/items-client';

const consumer = module('consumer');
registerItemsClient(consumer);
```

```go
import itemsclient "example.com/items/client"

consumer := app.NewModule("consumer")
itemsclient.RegisterItemsClient(consumer)
```

The framework resolves the service URL, client identity and credential sources
from the consumer's typed client configuration. The generated descriptor adds
the provider audience and credential profiles, safe retry and circuit policy,
deadlines, context and trace propagation, metrics, typed remote errors and the
declared transport. Tokens, API keys and named-header values remain runtime
configuration and never enter generated source or manifests.

Generated packages retain their explicit low-level constructors and optional
binding override for tests and external integrations. Production consumers use
the registration function and resolve the client through dependency injection.

## Workspace lifecycle

The workspace commands operate on every project of the workspace, regardless
of an impacted consumer-only selection. Sync and adopt read the Putnami project
index; the check reads the membership the job context carries:

| Command | Result |
| --- | --- |
| `putnami clientgen-sync` | Build all provider contracts without cache reuse, generate every configured target, synchronize tracked files and emit the coverage, lineage and adaptation report. |
| `putnami clientgen-check` | Judge every committed client against its committed manifest and the committed provider contract; fail on missing coverage, forged or extra generated files, a cache policy the manifest's runtime capabilities do not cover (`clientgen.unsupported-runtime-capability`), an unclassified external contract, or a handwritten first-party transport. Builds nothing, renders nothing. |
| `putnami clientgen-adopt` | Rewrite authored imports and construction onto generated bindings, for the entries two real emitter manifests prove on their own. |

Sync and adopt build the provider contracts through the Putnami that launched
them, so an impacted consumer-only selection cannot leave a provider on a stale
contract, and synchronize also rebuilds every project afterwards as its compile
gate. The check spawns no Putnami at all. A deleted generated directory is
recreated by the generator's declared dynamic output rather than hidden by a
cache hit.

### Drift is the generator's verdict

Whether a committed client is what the current contract generates is decided
where the client is written, not by this check. The four tasks that write
committed clients — `clientgen-go`, `clientgen-ts`, and the two in-session
writers of the same directories, `@putnami/go` `build-describe` and
`@putnami/typescript` `build-generate` — declare `drift: "fail"` on their
client output. The engine compares what each writes (or what a cache hit
restores) with the bytes present immediately before, and fails the task with
the diagnostic `generated-output-drift` naming the changed files; the worktree
then holds the regenerated client, and the remedy is a commit. A warm run and a
cold run reach one verdict, so a CI checkout of stale committed bytes fails
from the restore path
([`protocols/extension` ADR 0004](../../protocols/extension/doc/adr/0004-a-declared-output-may-police-its-own-drift.md),
[`ADR 0003`](doc/adr/0003-drift-is-the-generator-tasks-verdict.md)).

### The validate guard

The check is contributed to `putnami validate` with `workspace-once`
activation, so the gate every project already runs verifies the whole
workspace's committed clients exactly once — whatever the selection resolves
to, and whether or not the selected projects declare this extension. A
workspace opts out the ordinary way, by disabling the extension or the job.

`validate` depends on the `!clientgen` session barrier: the planner plans
`clientgen` for every selected project that declares this extension and orders
the guard after those leaves. A selected provider therefore regenerates every
target in place under the engine's drift judgment before the guard reads the
tree; a consumer-only selection plans no generation. In a workspace whose
`disable.tags` keeps a provider out of `--impacted`, that provider's drift is
judged when it is selected explicitly (`validate --projects <provider>`,
`clientgen-sync`); the guard still judges its committed bytes against its
committed manifest and contract on every run.

**What the guard reads.** The workspace's Git candidate cut and nothing else:
the tracked files and the untracked files no ignore rule excludes. Its member
projects are the ones the job context names, which the CLI resolves from
`putnami.workspace.json` and the scope manifests its includes name; the guard
never reads the project index. For each provider it reads the committed
`schema/openapi.json`, the committed `client.putnami.json` manifests under it —
which name its targets, since no build has written a generation contract on a
cold clone — and the inventories each project commits. A file Git ignores, such
as a provider's built `.gen/clientgen/config.json` or `.gen/schema/openapi.json`
or an ignored source, is absent, as it is from a clone; a project that commits
those `.gen` files has them read. That is what makes a cold clone and a tree the
session just built reach one verdict, and what removed the nested provider
build and the mirror render that cost ~10 s of every `putnami validate`.

**The same manifest is the workspace graph's contract edge.** A project whose
root commits a `client.putnami.json` is a generated client target, and the
provider is the project whose committed contract declares the `service.id` the
manifest names. Putnami derives that edge itself, from the same committed bytes
and through the same protocol decoder the guard reads a target with, so a
generated client is selected when its provider's committed contract changes
even though neither language's module graph contains the other. The edge
carries one input — the contract, whose digest the manifest states as
`contractSha256` — so it orders a client's generation after its provider's
contract producer and contributes nothing to the client's cache keys. Under
`--impacted`, a provider change that leaves `schema/openapi.json` alone selects
no client; a change to it opens the edge, which carries into each client what
the provider's change reaches, as a dependency edge does, and the trace names
the contract and its digest. See
`tooling/cli/doc/06-workspace-and-projects.md`.

**A contract that is empty by design.** A provider may declare its service
identity through the document-level `x-putnami-client` marker and no first-party
operation at all — every route owned by an external authority
(`x-putnami-external-contract`), or no route served. `go.putnami.dev/api` stages
nothing for it, and the guard reads that as a satisfied declaration: no
`clientgen.missing-manifest` for a declared target that never materialized, no
`clientgen.missing-config`, no `clientgen.no-targets`, and nothing counted as
required. The declaration is the committed contract sidecar itself, so a cold
clone reads it unchanged. It is exact, not a suppression: a contract with even
one first-party operation still owes its declared targets a client, a contract
whose operations discovery could not read is unreadable rather than empty, and a
target that IS committed is judged in full. Such a provider reports no target,
and a handwritten transport that reaches it is told to classify the callsite in
the consumer project's `clientgen.external.json` rather than to regenerate a
client that cannot exist.
See [`ADR 0004`](doc/adr/0004-an-empty-first-party-contract-is-a-declaration.md).

**What is cached and what is not.** The guard task itself
(`clientgen-workspace-check`) is keyed on the input `git:**`, which holds the
candidate cut it reads, and declares `cache: {enabled: true, noOutput: true}`.
Editing, adding, deleting or renaming a candidate moves the key; an ignored
file is neither read nor keyed. The key holds no commit, ref or absolute path,
so the same files at another commit or in another checkout replay the verdict.
See [`ADR 0006`](doc/adr/0006-the-guard-reads-and-keys-on-the-git-candidate-cut.md).

Every transport callsite the scanner finds is either associated with a
first-party provider — which fails, because a generated binding must replace it
— or claimed by one of the two inventories the project that holds the callsite
commits in its own directory: `<project>/clientgen.framework.json` and
`<project>/clientgen.external.json`. The guard reads the files of every member
project and merges them. A project's file is `protocolVersion: 2`, and its
entries name no project: the directory does, and the strict decoder refuses a
`project` member. `protocolVersion: 1`, one root file whose entries each name
their project, is the older layout: at the workspace root it fails with the
project file each entry moves to, and in a project directory it fails. A
version 2 file at the workspace root is read only when a project lives at the
root. See
[`ADR 0005`](doc/adr/0005-each-project-commits-its-own-client-inventories.md).

`clientgen.framework.json` covers a callsite no generated binding can replace,
under one of three statuses. `framework-runtime` is the transport a generated
binding itself dispatches to, and only `@putnami/client` and
`go.putnami.dev/client` may claim it. `transport-primitive` is a generic
transport whose endpoint and contract are supplied by the caller — a form
submission, a markdown component, a test harness. `pending-provider-contract`
is a first-party Putnami service whose provider-side client declaration does
not exist yet; it must name the operations it waits on and the work that
closes it, so it is a versioned state and not an exemption.

`clientgen.external.json` covers an adapter that speaks a contract someone else
owns — S3, Google Cloud Storage, the GCE metadata server, OAuth 2.0 and OIDC,
OTLP, the OCI distribution spec, the Go module proxy protocol, the npm registry
API — and names that authority.

Both inventories claim exact callsite expressions. An entry never names a
folder, a file or a transport as a class: a second call of the same symbol in a
listed file is a new callsite and fails, and an entry that matches nothing fails
too, so an inventory can only shrink. A callsite's identity is its normalized
expression, its ordinal among identical expressions, and the declarations in
that file of the identifiers it uses — so repointing `const url = ...` at a
different host revokes the entry, while an unrelated edit elsewhere in the file
does not.

The machine report counts descriptor operations per configured language target.
For every consumer that imports and calls a generated registration symbol, it
also records the provider, service, available operation, generated manifest,
consumer project, client and method symbols, auth profiles, stream direction and
transports. Those edges prove that the generated operation is available through
the registered binding; they do not claim that application code invoked every
operation.

When the source guard finds a handwritten first-party transport, the failing
diagnostic names the service and what to call instead: the generated package
and binding symbol when a target exists for the consumer's language, the
manifest to regenerate when the target is configured but not committed, or the
provider on which to declare the missing target. The report carries the same
bindings in a source-aware adaptation entry, but a failing job emits no report,
so the diagnostic is the channel a developer reads.

`clientgen-adopt` applies the entries two real emitter manifests can prove on
their own: a generated package that moved, and the client constructor or
registration call the product contract promises for a service. Both need the
contract bytes, the service and every operation to be identical on both sides.
Go rewrites a qualified selector on the exact identifier the file binds to that
import, reading the package clause the emitter wrote. TypeScript rewrites the
specifier and its uses, and refuses a source where the name is also bound
locally or used in object-shorthand position. Every file is staged before any
file is written, so an adoption that cannot finish migrates nothing. Argument,
credential and call-shape changes are never inferred; they stay in the queue.

## External contracts

An unmarked OpenAPI document is external only when its provider config says
`thirdParty: true`. Add one sorted entry to the `clientgen.external.json` in
the directory of the project that consumes the contract, here
`integrations/payment-provider/clientgen.external.json`, using the
[`clientgen-external-v2.json`](../../protocols/clientcontract/schemas/clientgen-external-v2.json)
schema:

```json
{
  "protocolVersion": 2,
  "contracts": [
    {
      "authority": "https://example.test/openapi.json",
      "adapter": "integrations/payment-provider/src/adapter.ts",
      "callsites": [{
        "path": "integrations/payment-provider/src/adapter.ts",
        "transport": "http",
        "symbol": "fetch",
        "fingerprint": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
      }],
      "owner": "payments",
      "tests": ["integrations/payment-provider/test/contract.test.ts"],
      "reason": "The upstream provider is outside this workspace"
    }
  ]
}
```

The adapter and every test path must exist. Each callsite fingerprint covers
the normalized transport expression, so formatting and comments do not churn
the inventory while changing its constructor or endpoint invalidates the
authority. A new or changed call in the same file remains an unknown blocking
transport. The check diagnostic prints the transport, symbol and fingerprint
needed for review. Stale entries fail. The directory that holds the file names
the project that consumes the external contract, and that project may itself be
a first-party provider: a workload that serves a Putnami API can still export
OTLP to a collector. The entry can never exempt calls to a marked first-party provider:
an `authority` that names a provider's service ID or project path fails with
`clientgen.first-party-external-bypass`, and a claimed callsite whose expression
names a first-party service still fails as a handwritten first-party client.

Low-level calls that implement the generated binding runtimes use the separate
`clientgen.framework.json` inventory of the runtime project, defined by
[`clientgen-framework-v2.json`](../../protocols/clientcontract/schemas/clientgen-framework-v2.json).
Its `runtime` must match the exact project identity (`@putnami/client`
or `go.putnami.dev/client`), and every adapter and callsite is verified with the
same exact identity rules. This keeps low-level runtime APIs available without
creating a project or file exemption that a consumer could copy.

## Execution model

The extension prepares its runtime and the Go emitter together through the
locked Putnami Go toolchain. The TypeScript emitter is resolved from the
Putnami-installed `@putnami/client` package shim. Generation does not depend on
a framework source checkout in the consumer workspace or a globally installed
`go`, `bun` or language generator.

Each target reports its configured directory through a language-specific output
port (`goClientOutput` or `typescriptClientOutput`). Putnami owns that whole
directory as a generated output. The strict `client.putnami.json` manifest
then hashes every emitted file and carries the public binding symbols and full
operation/transport inventory used by workspace verification.

When `.gen/clientgen/config.json` is missing, a target reports no directory
only if the provider commits no client in that language. A provider that
commits one fails generation instead and keeps its committed client. Both
project tasks read the project `gen` resource, so the planner never runs them
while another step of the same project rewrites `.gen`.

Both project tasks are cacheable, so their cache keys name every input that
decides an emitted byte: the clientgen configuration, the built contract, the
provider `package.json` and — for the TypeScript target — the workspace root
`package.json`, which decides whether the client depends on `@putnami/client`
through `catalog:`, a pinned version or `workspace:*`, and the Biome
configuration, its `extends` chain and the EditorConfig beside it, because the
emitter canonicalizes every file it writes with Biome. A configuration this
resolution cannot name as a workspace path (a package specifier, a path outside
the workspace) fails generation instead of being silently left out of the key.

### What is cached and what is not

| Task | Cached |
| --- | --- |
| `clientgen-go`, `clientgen-ts` | Yes — the keys above |
| `clientgen-sync`, `clientgen-adopt` | No — they rewrite the workspace |
| `clientgen-check` and the `validate` guard | Yes — the input `git:**` |

The workspace check reads the Git candidate cut and nothing else, and its key
is the input `git:**`, which holds that cut. Its read set is every production
source in the workspace, so no hand-written pattern list could be shown to cover
it; the cut can, because the check reads through it:

- the source scan visits the candidates each member project owns, skipping the
  build, install and dot-directories as before;
- the member projects come from the job context, resolved from committed
  workspace and scope manifests, never from the ignored
  `.putnami/workspace-index.json`;
- discovery reads a provider's `.gen/clientgen/config.json` and
  `.gen/schema/openapi.json` only when the project commits them; otherwise the
  committed sidecar and manifests decide, as they do on a cold clone.

A file Git ignores therefore changes neither the key nor the verdict, and every
candidate the verdict depends on moves the key. Outside a Git work tree the
check reads the disk, and no `git:` key exists to replay a verdict from.

On a miss the check pays its full cost, and the cost is bounded by doing less
work rather than by storing the answer:

- it builds nothing and renders nothing: drift is the generator task's own
  verdict, judged by the engine on its declared output (above);
- one workspace source scan per check answers both the consumer-edge question
  and the handwritten-transport question, instead of one scan each;
- a Go file is fully parsed only when its import block names a transport
  package, which the real parser decides by reading the header and stopping;
- a TypeScript file is masked once, and its import specifiers and transport
  callsites are read from that one mask;
- the global transports are looked for only in a masked source that names them;
- the expressions the scanner derives from identifier and module names are
  compiled once per process instead of once per file.

The report carries `timings` — one entry per phase, also emitted as
`clientgen.phase.<name>` metric events — so a slow guard is diagnosed from the
record rather than estimated. Measured on this repository before the nested
provider build and mirror render were removed (2151 indexed production
sources, 114 inventoried callsites, 35 consumer edges): the
nested provider build was 14.9 s and the mirror render 7.8 s of a 24.2 s guard
under load; the scan, which is what remains, was 1.3 s.

The versioned metadata and manifest formats are documented in
[`protocols/clientcontract`](../../protocols/clientcontract/README.md). The
accepted decisions are recorded in
[`ADR 0001`](doc/adr/0001-clients-are-generated-from-the-published-contract.md),
[`ADR 0003`](doc/adr/0003-drift-is-the-generator-tasks-verdict.md),
[`ADR 0004`](doc/adr/0004-an-empty-first-party-contract-is-a-declaration.md)
and
[`ADR 0006`](doc/adr/0006-the-guard-reads-and-keys-on-the-git-candidate-cut.md).

## Support status

- **Status**: `preview` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json). The extension is
  documented, runs under the repository gate, and contributes its check to
  `putnami validate` for every workspace that installs it.
- **Owner**: `@putnami/clientgen` (`tooling/clientgen-extension`).
- **What may still change**: the workspace command names, the shape of the
  machine report, and the two inventory schemas
  (`clientgen-framework-v2.json`, `clientgen-external-v2.json`). The emitters it
  drives are owned by `go.putnami.dev/api` and `@putnami/client`, whose own
  status is recorded separately.
- **Evidence**: the
  [first-party generated service client lifecycle](specs/cross-language-rest-clients.json)
  specification and its twenty-two requirements, the cache-key, manifest and
  packaging contract tests, the workspace discovery, render, drift and
  inventory tests, and the planner tests behind the validate guard's session
  barrier.

## License

[FSL-1.1-MIT](../../LICENSE.md)
