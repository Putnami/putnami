# @putnami/clientgen

`@putnami/clientgen` turns the contract emitted by a Putnami provider into
strict Go and TypeScript clients, then verifies their adoption across a
workspace. The provider declaration is the authority for schemas, typed errors,
security, service identity, idempotency, resilience, stream direction and the
REST/JSON, Connect/protobuf, SSE and WebSocket transports it exposes.

## Provider to consumer

Declare the provider API and enable its native client generator. In TypeScript,
add `clientGenerator()` to the application; in Go, add
`api.Clients(...).From(apiPlugin)`. The provider commits its marked contract as
`schema/openapi.json`, and a provider that generates an in-module Go client
commits its generation contract as `clientgen.json` at its root. First-party
mode is the default and rejects any contract semantic that the shared client IR
cannot represent.

**Migrate a contract committed under `.gen`.** Committing a file under
`.gen/clientgen` or `.gen/schema` is no longer supported, a breaking change
that ships in a minor release. The Go `build-describe` owns both directories
whole, so it fails while Git tracks a file there: the engine never empties an
emptied output that holds a tracked file (CLI ADR 0068). The TypeScript
`build-generate` owns `.gen` whole and deletes such a file. Migrate each
provider in three steps:

1. Run `git mv <project>/.gen/clientgen/config.json <project>/clientgen.json`.
2. Run `mkdir -p <project>/schema && git mv <project>/.gen/schema/openapi.json <project>/schema/openapi.json`.
   When `schema/openapi.json` already exists, run
   `git rm --cached <project>/.gen/schema/openapi.json` instead.
3. Ignore `.gen` in `.gitignore`, and drop any rule that re-includes a file
   under it, such as a `!config.json` carve-out. A workspace that does not
   ignore it holds
   each build's output there as a Git candidate, which the workspace check
   reports and which the clientgen keys read.

A plain `mv` leaves the old path in the Git index, and describe then fails
naming the `git rm --cached` to run. The workspace check reports each Git
candidate still under either tree as `clientgen.committed-generated-contract`,
with the command to run.

A client is a project of its own. Its `putnami.json` names the provider's
service in `generatedFrom`, with the language and, for Go, the package and
client names and the operations to leave out:

```json
{
  "name": "@acme/items-client",
  "generatedFrom": { "service": "items", "language": "ts" }
}
```

The `client-generate` command generates it from the provider's committed
`schema/openapi.json`, which Putnami resolves from the service. A Go client
gets `client.gen.go` at its root, and a TypeScript client gets its `src` tree.
A provider that keeps a Go client inside its own module commits a
`clientgen.json` whose `go.output` names that package's directory, and
`client-generate` writes `client.gen.go` there from the provider's own
committed contract. The `@putnami/go` and `@putnami/typescript` build, test,
lint, serve and package commands depend on `client-generate`, so building a
client or a project that depends on one generates it first:

```bash
putnami build --projects my-consumer
```

The generated files are built, not committed. See
[`ADR 0007`](doc/adr/0007-a-generated-client-is-built-not-committed.md).

Each generated package exports typed clients and registration functions.
Register the binding on the consumer module:

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
configuration and never enter generated source.

Generated packages retain their explicit low-level constructors and optional
binding override for tests and external integrations. Production consumers use
the registration function and resolve the client through dependency injection.

## Workspace lifecycle

The check operates on every project of the workspace, regardless of an
impacted consumer-only selection. It reads the membership the job context
carries:

| Command | Result |
| --- | --- |
| `putnami client-generate` | Generate each selected client project, and each selected provider's in-module client, from the committed contract. |
| `putnami clientgen-check` | Validate provider discovery, the two inventories and the committed contracts; fail on a tracked file that carries the generated marker (`clientgen.committed-generated-client`), an unclassified external contract, or a handwritten first-party transport. Builds nothing, generates nothing. |

The check spawns no Putnami at all. A deleted generated client is rendered
again by `client-generate`, or restored by a cache hit.

### The validate guard

The check is contributed to `putnami validate` with `workspace-once`
activation, so the gate every project already runs verifies the whole
workspace exactly once — whatever the selection resolves
to, and whether or not the selected projects declare this extension. A
workspace opts out the ordinary way, by disabling the extension or the job.

`validate` depends on nothing, so it plans no generation: the guard reads the
committed tree.

**What the guard reads.** The workspace's Git candidate cut and nothing else:
the tracked files and the untracked files no ignore rule excludes. Its member
projects are the ones the job context names, which the CLI resolves from
`putnami.workspace.json` and the scope manifests its includes name; the guard
never reads the project index. For each provider it reads the committed
`schema/openapi.json`, its committed `clientgen.json` when it has one, and the
inventories each project commits. A file Git ignores, such as a build's output under `.gen` or an
ignored source, is absent, as it is from a clone; a file a project commits
under `.gen/clientgen` or `.gen/schema` is reported as
`clientgen.committed-generated-contract`, never read as a contract. That is what makes a cold clone and
a tree the session just built reach one verdict, and what removed the nested
provider build and the mirror render that cost ~10 s of every
`putnami validate`.

**`generatedFrom` is the workspace graph's contract edge.** A project whose
`putnami.json` declares `generatedFrom` is a generated client, and its provider
is the project whose committed contract declares the named service. Putnami
derives that edge itself, so a client is selected when its provider's committed
contract changes even though neither language's module graph contains the
other. The client's identity folds the contract's digest and the generation
options, so they reach every cache key of the client's tasks. The edge orders
no job (CLI ADR 0087): generation reads a committed file, so it waits for no
provider task. Under `--impacted`, a provider change that leaves
`schema/openapi.json` alone selects no client. See
`tooling/cli/doc/06-workspace-and-projects.md`.

**A contract that is empty by design.** A provider may declare its service
identity through the document-level `x-putnami-client` marker and no first-party
operation at all — every route owned by an external authority
(`x-putnami-external-contract`), or no route served. `client-generate` writes
nothing for it. The declaration is the committed contract sidecar itself, so a
cold clone reads it unchanged. It is exact, not a suppression: a contract whose
operations discovery could not read is unreadable rather than empty. A
handwritten transport that reaches such a provider is told to classify the
callsite in the consumer project's `clientgen.external.json` rather than to
generate a client that cannot exist.
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

For every first-party provider, the machine report lists its clients: each
client project that names the provider's service in `generatedFrom`, and the
in-module Go client its `clientgen.json` declares, with the import path and the
binding symbols to call. The guard spells them from committed files only: the
contract, the generation options, and the client's `go.mod` or `package.json`.

When the source guard finds a handwritten first-party transport, the failing
diagnostic names the service and what to call instead: the generated package
and binding symbol when a client exists for the consumer's language, or the
client project to declare with `generatedFrom` when none does. The report
carries the same bindings in a source-aware adaptation entry, but a failing job
emits no report, so the diagnostic is the channel a developer reads.

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

Each generation task claims only what its emitter writes: `client.gen.go` for
a Go client project, `src` for a TypeScript one, and, for an in-module target,
the `client.gen.go` it reports through the `moduleClientOutput` port, since
`clientgen.json` chooses its directory. No output polices drift, because none
is committed. Each output is read by the owning project's dependents, so the
planner runs none of their jobs while the generator rewrites it.

The generation tasks are cacheable, so their keys name every input that decides
an emitted byte. A client project keys on its `putnami.json`, which carries
the generation options, and on its `go.mod` or `package.json`. Its provider's
contract reaches the key through the client's identity. The in-module target
keys on the provider's `clientgen.json` and `schema/openapi.json`. The
TypeScript target also keys on the workspace root `package.json`, which decides
whether the client depends on `@putnami/client` through `catalog:`, a pinned
version or `workspace:*`, and on the Biome configuration, its `extends` chain
and the EditorConfig beside it, because the emitter canonicalizes every file it
writes with Biome. A configuration this resolution cannot name as a workspace
path (a package specifier, a path outside the workspace) fails generation
instead of being silently left out of the key. No key hashes a file a build
writes under `.gen`.

### What is cached and what is not

| Task | Cached |
| --- | --- |
| `client-generate-go`, `client-generate-ts`, `client-generate-module` | Yes — the keys above |
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
- discovery reads a provider's committed `clientgen.json` and sidecar, as it
  does on a cold clone; a file under `.gen/clientgen` or
  `.gen/schema` is never a contract.

A file Git ignores therefore changes neither the key nor the verdict, and every
candidate the verdict depends on moves the key. Outside a Git work tree the
check reads the disk, and no `git:` key exists to replay a verdict from.

On a miss the check pays its full cost, and the cost is bounded by doing less
work rather than by storing the answer:

- it builds nothing and generates nothing;
- one workspace source scan per check finds both the tracked generated files
  and the handwritten transports;
- a Go file is fully parsed only when its import block names a transport
  package, which the real parser decides by reading the header and stopping;
- a TypeScript file is masked once, and its transport callsites are read from
  that one mask;
- the global transports are looked for only in a masked source that names them;
- the expressions the scanner derives from identifier and module names are
  compiled once per process instead of once per file.

The report carries `timings` — one entry per phase, also emitted as
`clientgen.phase.<name>` metric events — so a slow guard is diagnosed from the
record rather than estimated. Measured on this repository before the nested
provider build and mirror render were removed (2151 indexed production
sources, 114 inventoried callsites): the
nested provider build was 14.9 s and the mirror render 7.8 s of a 24.2 s guard
under load; the scan, which is what remains, was 1.3 s.

The versioned metadata format is documented in
[`protocols/clientcontract`](../../protocols/clientcontract/README.md). The
accepted decisions are recorded in
[`ADR 0001`](doc/adr/0001-clients-are-generated-from-the-published-contract.md),
[`ADR 0003`](doc/adr/0003-drift-is-the-generator-tasks-verdict.md),
[`ADR 0004`](doc/adr/0004-an-empty-first-party-contract-is-a-declaration.md),
[`ADR 0006`](doc/adr/0006-the-guard-reads-and-keys-on-the-git-candidate-cut.md)
and
[`ADR 0007`](doc/adr/0007-a-generated-client-is-built-not-committed.md).

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
  specification and its requirements, the cache-key, extension manifest and
  packaging contract tests, the generation handler tests, the workspace discovery and
  inventory tests, and the planner tests behind the validate guard.

## License

[FSL-1.1-MIT](../../LICENSE.md)
