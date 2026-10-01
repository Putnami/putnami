# Health checks

Use the [`platform()` plugin](./26-platform-endpoints.md) for the standard operational surface shared by the Go and TypeScript frameworks.

```ts
import { application, http, platform } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(platform());

await app.start();
```

The plugin exposes `/livez`, `/healthz`, `/readyz`, and `/version`. It auto-discovers `HealthChecker` and `ReadinessChecker` implementations from the module tree and accepts explicit or dependency-injected probes.

Configure deployment liveness against `/livez` and readiness against `/readyz`. Use `/healthz` for aggregate diagnostic health. Set `prefix: '/_'` when operational endpoints must be namespaced.

See [Platform endpoints](./26-platform-endpoints.md) for endpoint envelopes, probe authoring, timeouts, and cross-language conformance.

## Related guides

- [Telemetry](/docs/frameworks/typescript/telemetry)
- [Plugins & lifecycle](/docs/frameworks/typescript/plugins-and-lifecycle)
- [HTTP & middleware](/docs/frameworks/typescript/http-and-middleware)
- [Gate production builds with doctor](/docs/how-to/gate-production-builds-with-doctor)
