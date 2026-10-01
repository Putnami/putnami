import type { ContainerContext } from '@putnami/runtime';
import type { HealthContribution, Module, Plugin } from '../application';
import { HttpPlugin } from '../http/http.plugin';
import { HttpResponse } from '../http/http-response';
import { isHealthChecker, isReadinessChecker } from './checker';
import {
  DEFAULT_PROBE_TIMEOUT_MS,
  type Envelope,
  HTTP_STATUS_OK,
  HTTP_STATUS_UNAVAILABLE,
  PATH_HEALTHZ,
  PATH_LIVEZ,
  PATH_READYZ,
  PATH_VERSION,
  STATUS_DEGRADED,
  STATUS_OK,
  STATUS_UNAVAILABLE,
  type VersionInfo,
  joinPrefix,
  normalizePrefix,
  validateProbeName,
} from './protocol';

/**
 * Per-probe callback shape used by explicit registration. Mirrors the
 * canonical {@link HealthChecker.checkHealth} signature: receive an
 * {@link AbortSignal}, return (or throw) nothing on success.
 */
export type ProbeFunction = (signal: AbortSignal) => Promise<void> | void;

/**
 * Synthesized `/readyz` check value for a probe named in
 * {@link PlatformOptions.required} that was never registered or
 * discovered. Surfaces in the degraded envelope like a probe's error
 * message. Kept byte-identical to the Go runtime (go/framework/platform)
 * so the same missing-required name yields the same envelope in both
 * runtimes — the protocol's `platform.missing_probe` taxonomy entry
 * (`ERROR_CODE_MISSING_PROBE` in `./protocol`), expressed through the
 * existing envelope with no wire change.
 */
const MISSING_REQUIRED_PROBE_MESSAGE = 'required probe not registered or discovered';

/**
 * Throws if a probe name would make a /healthz or /readyz envelope fail the
 * protocol's own validator. Mirrors go/framework/platform's `validateProbeName`
 * guard so a non-conforming name (registered, discovered, or required) fails
 * fast at start instead of emitting a self-inconsistent envelope at request time.
 */
function assertProbeName(name: string): void {
  const diagnostics = validateProbeName(name);
  if (diagnostics.length > 0) {
    throw new Error(
      `platform: probe name does not match the canonical pattern ^[a-z0-9][a-z0-9_./-]{0,63}$: ${JSON.stringify(name)}`,
    );
  }
}

export interface PlatformOptions {
  /**
   * Path prefix for every endpoint. Empty (default) mounts at root
   * (`/healthz`, `/livez`, …). Set to e.g. `"/_"` to namespace under
   * `/_/healthz`. Normalised to canonical form before mounting.
   */
  prefix?: string;

  /**
   * Per-probe timeout. Defaults to 5 seconds (the protocol default).
   * Probes that exceed this surface as a failing check.
   */
  probeTimeoutMs?: number;

  /**
   * Readiness probe names the workload treats as mandatory. Any name here
   * that is not among the registered or auto-discovered readiness probes
   * when `/readyz` runs makes the endpoint report `degraded` with a
   * synthesized failing check for that name — a missing required
   * dependency reads exactly like a failing one, so orchestrators drain
   * traffic from a workload that is wired incompletely. Names that ARE
   * registered are unaffected: their probe runs normally. `/healthz`
   * ignores this list; readiness is where "declared but never wired up"
   * must fail closed. Expressed through the existing degraded envelope
   * (the protocol's `platform.missing_probe` taxonomy entry) — not a wire
   * change, no protocol bump.
   */
  required?: string[];

  /**
   * Build metadata returned by /version. Empty fields are omitted from
   * the response. There is no debug-info fallback in Node — the caller
   * embeds version data at build time (e.g. via `import.meta.env`).
   */
  version?: VersionInfo;
}

/**
 * Mounts the standard operational HTTP surface:
 *
 * - `/livez`   — lightweight liveness (always 200 when the handler runs)
 * - `/healthz` — liveness aggregate, runs every {@link HealthChecker} probe
 * - `/readyz`  — readiness aggregate, runs every {@link ReadinessChecker} probe
 * - `/version` — build metadata as JSON
 *
 * Probes are auto-discovered from the module tree at warmup: any plugin
 * implementing {@link HealthChecker} or {@link ReadinessChecker} is
 * registered under its `name`. Workloads can override or augment via
 * {@link PlatformPlugin.addHealthChecker} / `addReadinessChecker` —
 * explicit registrations win over auto-discovered ones of the same name.
 *
 * @example
 * ```typescript
 * const app = application()
 *   .use(database({ … }))   // implements HealthChecker
 *   .use(http({ port: 3000 }))
 *   .use(platform({ version: { name: 'my-service', version: '1.0.0' } }));
 * ```
 */
export class PlatformPlugin implements Plugin {
  private ready = false;
  private readonly prefix: string;
  private readonly probeTimeoutMs: number;
  private readonly version: VersionInfo;
  private readonly required: readonly string[];
  private readonly healthCheckers = new Map<string, ProbeFunction>();
  private readonly readinessCheckers = new Map<string, ProbeFunction>();
  private root?: Module;

  constructor(options: PlatformOptions = {}) {
    this.prefix = normalizePrefix(options.prefix ?? '');
    this.probeTimeoutMs = options.probeTimeoutMs ?? DEFAULT_PROBE_TIMEOUT_MS;
    this.version = options.version ?? {};
    this.required = options.required ?? [];
  }

  /**
   * Register an explicit liveness probe. Use for probes not owned by a
   * plugin (an external URL, ad-hoc check). Explicit registrations
   * override auto-discovered probes of the same name.
   */
  addHealthChecker(name: string, probe: ProbeFunction): this {
    this.healthCheckers.set(name, probe);
    return this;
  }

  /** Register an explicit readiness probe. Same precedence rule as health. */
  addReadinessChecker(name: string, probe: ProbeFunction): this {
    this.readinessCheckers.set(name, probe);
    return this;
  }

  async warmup(owner: Module): Promise<void> {
    this.root = owner.getRoot();
    // Auto-discover from the WHOLE module tree, starting at root —
    // matches the Go side and lets a platform plugin in a child module
    // still see capabilities elsewhere in the app.
    for (const { plugin } of this.root.collectPlugins()) {
      if (isHealthChecker(plugin) && !this.healthCheckers.has(plugin.name)) {
        this.healthCheckers.set(plugin.name, (signal) => plugin.checkHealth(signal));
      }
      if (isReadinessChecker(plugin) && !this.readinessCheckers.has(plugin.name)) {
        this.readinessCheckers.set(plugin.name, (signal) => plugin.checkReadiness(signal));
      }
    }
    this.discoverHealthContributions();

    const httpPlugin = await owner.ensurePlugin(HttpPlugin);
    httpPlugin.prepend(async (ctx, next) => {
      if (ctx.method !== 'GET') return next();

      // ctx.path() returns the request path; compare against the
      // configured mount paths via joinPrefix so the matching shape
      // matches the published helper.
      const path = `/${ctx.path()}`.replace(/^\/+/, '/');

      if (path === joinPrefix(this.prefix, PATH_LIVEZ)) {
        return this.livez();
      }
      if (path === joinPrefix(this.prefix, PATH_HEALTHZ)) {
        this.discoverHealthContributions();
        return this.aggregate(this.healthCheckers);
      }
      if (path === joinPrefix(this.prefix, PATH_READYZ)) {
        this.discoverHealthContributions();
        return this.aggregate(this.readinessCheckers, this.required);
      }
      if (path === joinPrefix(this.prefix, PATH_VERSION)) {
        return HttpResponse.json(this.version);
      }
      return next();
    });
  }

  async start(owner?: Module): Promise<void> {
    if (owner) {
      this.root = owner.getRoot();
    }
    this.discoverHealthContributions();
    // Name-check every probe that can surface as a /healthz or /readyz checks
    // key before flipping ready: a non-conforming name would make the envelope
    // fail the protocol's own validator. Mirrors go/framework/platform Start —
    // discovered/registered probe names AND the required list are validated, so
    // a misconfigured name fails fast here instead of at request time.
    for (const name of this.healthCheckers.keys()) {
      assertProbeName(name);
    }
    for (const name of this.readinessCheckers.keys()) {
      assertProbeName(name);
    }
    for (const name of this.required) {
      assertProbeName(name);
    }
    this.ready = true;
  }

  async stop(): Promise<void> {
    this.ready = false;
  }

  /**
   * /livez handler — intentionally minimal. If the request reached
   * this handler, the process is alive; k8s liveness probes that
   * depend on `ready` here cause restart loops during startup/drain.
   */
  private livez(): HttpResponse {
    return HttpResponse.json({ status: STATUS_OK } satisfies Envelope, { status: HTTP_STATUS_OK });
  }

  private async aggregate(
    checkers: Map<string, ProbeFunction>,
    required: readonly string[] = [],
  ): Promise<HttpResponse> {
    if (!this.ready) {
      return HttpResponse.json({ status: STATUS_UNAVAILABLE } satisfies Envelope, {
        status: HTTP_STATUS_UNAVAILABLE,
      });
    }

    // Required readiness names that were never registered or discovered
    // fail closed: a workload that declares a dependency required but never
    // wires it up must not report ready. De-duplicated and order-preserving
    // for a stable response. (Health passes no required list, so this is
    // always empty for /healthz.)
    const missing = required.filter((name, idx) => !checkers.has(name) && required.indexOf(name) === idx);

    if (checkers.size === 0 && missing.length === 0) {
      return HttpResponse.json({ status: STATUS_OK } satisfies Envelope, {
        status: HTTP_STATUS_OK,
      });
    }

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.probeTimeoutMs);
    try {
      const checks: Record<string, string> = {};
      let allHealthy = true;
      // Run probes in parallel; each probe is independently bounded so a
      // probe that ignores the abort signal cannot stall the response.
      const results = await Promise.allSettled(
        Array.from(checkers.entries()).map(([name, probe]) => this.runProbe(name, probe, controller.signal)),
      );

      const entries = Array.from(checkers.keys());
      results.forEach((result, idx) => {
        const name = entries[idx];
        if (!name) return;
        if (result.status === 'fulfilled') {
          checks[name] = 'ok';
        } else {
          allHealthy = false;
          checks[name] = errorMessage(result.reason);
        }
      });

      // Synthesize a failing entry for every missing required probe. Added
      // after the real results so it never collides with a registered probe
      // of the same name (a required name that IS registered ran above).
      for (const name of missing) {
        checks[name] = MISSING_REQUIRED_PROBE_MESSAGE;
        allHealthy = false;
      }

      const envelope: Envelope = {
        status: allHealthy ? STATUS_OK : STATUS_DEGRADED,
        checks,
      };
      return HttpResponse.json(envelope, {
        status: allHealthy ? HTTP_STATUS_OK : HTTP_STATUS_UNAVAILABLE,
      });
    } finally {
      clearTimeout(timer);
    }
  }

  /**
   * Race a probe against the shared abort signal. The signal aborts when
   * `probeTimeoutMs` elapses; probes that respect it can short-circuit
   * early, but probes that ignore it (e.g. drivers that don't accept an
   * AbortSignal — see sql.plugin.ts and postgres.js) still reject here
   * rather than hanging the aggregate response indefinitely.
   *
   * The abort listener is removed once the probe settles to avoid
   * accumulating dead listeners on the long-lived controller signal
   * (note: in this aggregate path the controller is per-request so the
   * leak window is short, but the cleanup keeps the contract clear).
   */
  private runProbe(name: string, probe: ProbeFunction, signal: AbortSignal): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const timeoutError = () => new Error(`probe '${name}' timed out after ${this.probeTimeoutMs}ms`);

      if (signal.aborted) {
        reject(timeoutError());
        return;
      }

      const onAbort = (): void => reject(timeoutError());
      signal.addEventListener('abort', onAbort, { once: true });

      Promise.resolve()
        .then(() => probe(signal))
        .then(
          () => {
            signal.removeEventListener('abort', onAbort);
            resolve();
          },
          (err) => {
            signal.removeEventListener('abort', onAbort);
            reject(err);
          },
        );
    });
  }

  private discoverHealthContributions(): void {
    const root = this.root;
    if (!root) return;
    for (const contribution of root.collectHealthContributions()) {
      const checkers = contribution.kind === 'readiness' ? this.readinessCheckers : this.healthCheckers;
      if (!checkers.has(contribution.name)) {
        checkers.set(contribution.name, (signal) => runInjectedProbe(root, contribution, signal));
      }
    }
  }
}

/** Build a {@link PlatformPlugin}. */
export function platform(options: PlatformOptions = {}): PlatformPlugin {
  return new PlatformPlugin(options);
}

function errorMessage(reason: unknown): string {
  if (reason instanceof Error) return reason.message;
  if (typeof reason === 'string') return reason;
  try {
    return JSON.stringify(reason);
  } catch {
    return String(reason);
  }
}

function runInjectedProbe(root: Module, contribution: HealthContribution, signal: AbortSignal): Promise<void> | void {
  const context = activeContext(root, contribution.name);
  const deps = contribution.deps.map((token) => context.get(token));
  return contribution.probe(...deps, signal);
}

function activeContext(root: Module, probe: string): ContainerContext {
  const appLike = root as Module & {
    getActiveContext?: () => ContainerContext | undefined;
    context?: ContainerContext;
  };
  const context = appLike.getActiveContext?.() ?? appLike.context;
  if (!context) {
    throw new Error(`health probe '${probe}' cannot resolve dependencies before the application context starts`);
  }
  return context;
}
