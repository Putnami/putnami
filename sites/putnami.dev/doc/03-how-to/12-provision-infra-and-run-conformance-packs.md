# Provision declared infrastructure and run conformance packs

You will make a project declare the infrastructure its tests need, have `putnami test` provision it automatically, and certify the project against a shared conformance pack with one committed line — no per-project database wiring, no hardcoded connections.

The idea: your project *declares* what it needs (a Postgres datasource), and the toolchain *provisions* it. Locally, `putnami test` starts a throwaway container and injects a binding; in CI, it consumes the binding your pipeline already supplies. Your tests never probe for a server or hardcode a port. On top of that, a *conformance pack* lets you certify a whole family of behaviors (transactions, health probes, …) against the provisioned infrastructure with a single opt-in line.

## Steps

### 1) Declare the infrastructure your tests need

Add `infra/requirements.json` at the project root, listing the datasources your tests use:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-infra.json",
  "protocolVersion": 2,
  "databases": [
    {
      "name": "default",
      "engine": "postgres",
      "schemas": ["public"]
    }
  ]
}
```

That is the whole contract. The requirement travels with the project through its dependency graph, so a library that declares a datasource pulls the requirement into every workload that depends on it.

### 2) Let `putnami test` provision it

Run your tests normally:

```bash
putnami test
```

`putnami test` reads the declared requirements and resolves an infrastructure policy before running anything:

| mode | when | behavior |
|---|---|---|
| `auto` | local default | uses a Postgres named in `PUTNAMI_TEST_PG_URL` when present; otherwise provisions a throwaway Docker container and hands tests a `DATABASE_TEST_BINDINGS` binding. The container stays running for the next run to reuse |
| `require` | CI default | uses a Postgres named in `PUTNAMI_TEST_PG_URL` to create each project's binding; otherwise consumes the `DATABASE_TEST_BINDINGS` your pipeline injects, and fails loudly if neither is present |
| `skip` | opt out | injects nothing; tests gated on the binding skip |

Override the policy with the `--infra` flag or the `test.infra` config key (flag wins):

```bash
putnami test --infra require   # consume an external binding, fail if absent
putnami test --infra skip       # never provision
putnami test --infra-down       # tear down any leftover provisioned containers
```

An externally set `DATABASE_TEST_BINDINGS` always wins: when the variable is present, `putnami test` provisions nothing and your binding flows through untouched.

Alternatively, a pipeline can start Postgres and set `PUTNAMI_TEST_PG_URL` to its
`postgres://` or `postgresql://` URL. When no complete binding is injected,
`putnami test` verifies that server and writes a binding for each project's own
dependency closure; this works in CI under the default `require` mode. The server
remains pipeline-owned and is never removed by `--infra-down`.

The provisioned binding never travels as an environment variable of its own. It is written to a private, owner-only file that exists only for the duration of the run, and the CLI destroys it when the run ends — so a credential is not inherited by every descendant process, does not appear in `ps`, and cannot end up in a build cache entry. If a run is killed before it can clean up, the next `putnami test` reclaims the leftovers before it provisions anything.

Because the binding is provisioned from your *declared* requirements, your tests must consume it through the framework's datasource resolution rather than a hardcoded connection — a provisioned binding wins, and your committed test config is the fallback:

```ts
// Resolve the datasource through the framework. A provisioned binding wins;
// conf/.env.test.yaml is the fallback. No hardcoded host/port in the test.
import { database } from '@putnami/database';

const sql = await database('default');
await sql`SELECT 1`;
```

A suite that creates its own isolated databases with the test provider names the datasources it uses, so it does not pay for every datasource in the binding. The provider creates, migrates and drops only those, and the binding it returns carries only them:

```ts
import { provision } from '@putnami/database';

const db = await provision({ datasources: ['default'] });
```

In Go, pass `testprovider.Options{Datasources: []string{"default"}}` to `testprovider.Provision`. A name the binding lacks follows the binding's mode: `skip` skips the suite, `require` fails and names the datasource.

### 3) Opt into a conformance pack (one line)

A conformance pack is a shared, cross-language test corpus a framework ships so every adopter certifies the same behavior against its own provisioned infrastructure. Opt in with a single committed line in a test file.

TypeScript — certify the transaction / concurrency corpus:

```ts
// test/conformance.test.ts
import { registerConformanceTests } from '@putnami/database/conformance';

registerConformanceTests();
```

Go — the same corpus, same manifest, from the sibling runner:

```go
// conformance_test.go
package myapp_test

import (
	"testing"

	"go.putnami.dev/database/conformance"
)

func TestTransactionConformance(t *testing.T) {
	conformance.Run(t)
}
```

Both runners provision through the same `DATABASE_TEST_BINDINGS` path from step 2, so the pack runs live against the auto-provisioned database and skips cleanly when no binding is present. Health-probe packs follow the same shape (`registerHealthConformanceTests()` in TypeScript; the Go health runner in `go.putnami.dev/app/conformance`).

### 4) Understand the pack-manifest convention

A framework advertises a pack with a committed `conformance/pack.json` next to its runner:

```json
{
  "id": "putnami.transaction.conformance",
  "corpus": "manifest.json",
  "capabilityKinds": ["datasource"],
  "languages": ["go", "typescript"]
}
```

| field | meaning |
|---|---|
| `id` | stable pack identity |
| `capabilityKinds` | the capability kinds the pack certifies (e.g. `datasource`, `health`) |
| `languages` | languages with an exported runner |
| `corpus` | optional path to a committed corpus fixture; omit when the corpus is a runtime artifact |

When a pack is reachable through your project's dependency graph and its `capabilityKinds` intersect the kinds your project declares in `schema/capabilities.json`, the pack is *applicable* to your project. Add the opt-in line from step 3 to certify it.

Applicable packs — opted-in or not — are recorded in the project's agent-context document (`putnami context`), which is where a tool or a reviewer can see which corpora a project could certify and which it already does.

You now have a project that declares its own infrastructure, provisions it automatically under `putnami test`, and certifies its behavior against a shared conformance pack — with no per-project database plumbing and no hardcoded connections.
