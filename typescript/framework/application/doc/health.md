# Platform health endpoints

Use the `platform()` plugin to expose Putnami's standard operational HTTP surface. It provides liveness, health, readiness, and version endpoints with protocol-compatible response envelopes.

```typescript
import { application, http, platform } from '@putnami/application';

const app = application()
  .use(http())
  .use(platform());
```

The default routes are:

| Route | Purpose |
| --- | --- |
| `GET /livez` | Lightweight liveness response |
| `GET /healthz` | Aggregate health checks |
| `GET /readyz` | Aggregate readiness checks |
| `GET /version` | Build metadata |

Set `prefix` when an operational namespace is required:

```typescript
platform({ prefix: '/_' });
```

This mounts `/_/livez`, `/_/healthz`, `/_/readyz`, and `/_/version`.

Plugins implementing `HealthChecker` or `ReadinessChecker` are discovered automatically. Application modules can also contribute dependency-injected probes, and the platform plugin accepts explicit checks for resources that are not owned by another plugin.

```typescript
const probes = platform({ required: ['database'] })
  .addHealthChecker('upstream', async (signal) => {
    await checkUpstream(signal);
  })
  .addReadinessChecker('database', async (signal) => {
    await checkDatabase(signal);
  });
```

Configure deployment probes against `/livez` and `/readyz`; use `/healthz` for diagnostic aggregate health.
