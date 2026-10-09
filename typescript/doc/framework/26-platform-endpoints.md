# Platform endpoints

`@putnami/application`'s `platform()` plugin mounts the standard operational HTTP surface every workload needs — liveness, readiness, version metadata — and auto-discovers per-plugin probes from the module tree.

It replaces per-workload operational boilerplate with one plugin you opt in to.

## Plugin setup

```ts
import { application, http, platform } from '@putnami/application';
import { sql } from '@putnami/database';

const app = application()
  .use(sql())                    // implements HealthChecker
  .use(http({ port: 3000 }))
  .use(platform({
    version: { name: 'my-service', version: '1.0.0' },
  }));

await app.start();
```

`GET /healthz` now reports:

```json
{
  "status": "ok",
  "checks": { "database": "ok" }
}
```

— the `database` probe shows up automatically because `sql()` implements `HealthChecker`.

## Endpoints

| Path | Purpose |
| ---- | ------- |
| `/livez` | Lightweight liveness — `200 {"status":"ok"}` whenever the handler can run. No probes, no flags. Safe for Kubernetes liveness probes. |
| `/healthz` | Liveness aggregate — `200` when running and every `HealthChecker` probe passes; `503 {"status":"unavailable"}` before `start()` and after `stop()`; `503 {"status":"degraded","checks":{…}}` when any probe fails. |
| `/readyz` | Readiness aggregate — same shape as `/healthz`, driven by `ReadinessChecker` probes, and `503 {"status":"unavailable"}` until the application completed startup: every plugin `start()` resolved. The endpoint to use as a Kubernetes readiness probe. |
| `/version` | Build metadata as JSON. Returns the `VersionInfo` you configure; empty fields are omitted. |

Default mount is root. Set `prefix` to namespace (`prefix: '/_'` mounts `/_/healthz`, `/_/livez`, …).

## Capability interfaces

The platform plugin discovers two interfaces by walking the module tree from the root and registers each implementation under its `name`.

```ts
import type { HealthChecker, ReadinessChecker } from '@putnami/application';

class CacheClient implements Plugin, HealthChecker {
  readonly name = 'cache';

  async checkHealth(signal: AbortSignal): Promise<void> {
    await this.ping(signal); // throws on failure
  }
}
```

| Interface | Semantics | Failure consequence (k8s) |
| --------- | --------- | ------------------------- |
| `HealthChecker` | Dependency is broken in a way only a restart fixes | Pod restart |
| `ReadinessChecker` | Dependency is temporarily down — drain traffic but don't restart | Traffic drained, no restart |

A single plugin can implement both: `checkHealth` contributes to `/healthz`, `checkReadiness` contributes to `/readyz`.

Implementations must be safe to call concurrently and respect `AbortSignal` cancellation. The plugin enforces a per-request `probeTimeoutMs` (default `5000`) so a hung probe can't stall the endpoint.

**Built-in contributor:** `@putnami/database`'s `sql()` plugin implements `HealthChecker` — `SELECT 1` against the default datasource shows up under the `database` key on `/healthz` automatically when both plugins are mounted.

For checks that only need DI-managed dependencies, contribute an injected function from the owning module/application:

```ts
const app = application()
  .provide(Pools)
  .use(http())
  .use(platform());

app.health.contributeReadiness('platform_configs', [Pools], async (pools, signal) => {
  await pools.for('platform_configs').ping({ signal });
});
```

## Explicit probes

For probes not owned by a plugin (an external URL, an ad-hoc check), register directly:

```ts
const platformPlugin = platform()
  .addHealthChecker('upstream', async (signal) => {
    const res = await fetch('https://example.com/health', { signal });
    if (!res.ok) throw new Error(`upstream returned ${res.status}`);
  })
  .addReadinessChecker('warm', async () => {
    if (!cacheWarmed) throw new Error('cache warming');
  });
```

Explicit registrations win over auto-discovered probes of the same name.

## Configuration

```ts
platform({
  prefix: '/_',                            // default ''
  probeTimeoutMs: 3_000,                   // default 5_000
  version: { name: 'my-service', version: '1.0.0' },
});
```

## Cross-language contract

The endpoint paths, response envelope, status mapping, capability semantics, probe behaviour, and discovery rules are pinned by the [platform protocol](https://github.com/Putnami/putnami/tree/main/protocols/platform). Both the Go and TS runtimes ship against the same contract — the TS protocol validators (`validateEnvelope`, `validateProbeName`, `validatePrefix`) are exposed for application-level conformance tests.
