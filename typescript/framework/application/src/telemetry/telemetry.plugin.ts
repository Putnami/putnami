/**
 * Telemetry Plugin — Main Orchestrator
 *
 * Lifecycle:
 *   warmup  → resolve the collector, init the collector, instrument HTTP outermost
 *   start   → start periodic flush timer
 *   stop    → final flush, clear timer, tear down collector
 *
 * The flush timer drains completed per-second buckets from the collector,
 * renders them as OTLP/JSON, and POSTs them to a standard OTLP/HTTP collector's
 * /v1/metrics and /v1/traces. Each push asks the registered telemetry token
 * source for its bearer. Logs go through an OtlpLogSink on the root logger,
 * which `application()` attaches when the environment names a collector.
 *
 * `application()` adds the plugin `telemetryFromEnvironment()` builds when the
 * environment names a collector and the composition holds no `telemetry()`.
 */

import { type InferConfig, useConfig, useLogger } from '@putnami/runtime';
import type { Module, Plugin } from '../application';
import type { HttpMiddleware } from '../http/http-middleware.type';
import { HttpPlugin } from '../http/http.plugin';
import type { MetricsBucket, TelemetrySpanRecord } from './telemetry.collector';
import { TelemetryCollector } from './telemetry.collector';
import { TelemetryConfig, type TelemetryOptions } from './telemetry.config';
import { CollectorAccess } from './telemetry.credential';
import { environmentEndpoint, serviceIdentity } from './telemetry.environment';
import { TelemetryMiddleware } from './telemetry.middleware';
import { sendMetrics, sendTraces } from './telemetry.sender';
import { clearCollector, setCollector } from './telemetry.utils';

/** Where a running plugin pushes, and as whom. */
interface TelemetryTarget {
  /** The collector and the credential of each push. */
  access: CollectorAccess;
  /** The OTLP resource's service.name. */
  app: string;
  resourceAttributes: Record<string, string>;
}

/** Orchestrates telemetry collection, periodic flushing, and shutdown draining. */
export class TelemetryPlugin implements Plugin {
  private collector: TelemetryCollector | undefined;
  private flushTimer: ReturnType<typeof setInterval> | undefined;
  private options: TelemetryOptions;
  private target: TelemetryTarget | undefined;
  private middleware: HttpMiddleware | undefined;
  private readonly environment: boolean;

  /** Runtime flush interval. */
  private flushIntervalS = 30;

  constructor(options: TelemetryOptions = {}, internal: { fromEnvironment?: boolean } = {}) {
    this.options = options;
    this.environment = internal.fromEnvironment === true;
  }

  /** True for the plugin `application()` added because the environment names a collector. */
  get fromEnvironment(): boolean {
    return this.environment;
  }

  // -----------------------------------------------------------------------
  // Lifecycle
  // -----------------------------------------------------------------------

  async warmup(app: Module): Promise<void> {
    const config = useConfig(TelemetryConfig, { confInit: this.options });
    const target = this.resolveTarget(config);
    if (!target) return;

    this.target = target;
    this.flushIntervalS = config.flushIntervalS;

    this.collector = new TelemetryCollector();
    setCollector(this.collector);

    this.middleware = TelemetryMiddleware(this.collector);
    this.instrument(app);
  }

  /**
   * Put the telemetry middleware outermost on every server the application
   * composes, in a sub-module too, so a request an inner middleware refuses is
   * still counted. Telemetry creates no server; without one, the collector
   * serves custom metrics. `Application.prepare()` calls it again once every
   * plugin has warmed up, so a server another plugin creates in its own warmup
   * is instrumented whatever the composition order. Idempotent.
   */
  instrument(app: Module): void {
    const middleware = this.middleware;
    if (!middleware) return;
    for (const { plugin } of app.getRoot().collectPlugins()) {
      if (plugin instanceof HttpPlugin) plugin.useOutermost(middleware);
    }
  }

  async start(app: Module): Promise<void> {
    if (!this.collector || !this.target) return;
    const endpoint = this.target.access.endpoint;

    this.flushTimer = setInterval(() => {
      this.flush().catch((error) => {
        useLogger('telemetry').warn('Flush failed', { endpoint, error });
      });
    }, this.flushIntervalS * 1000);

    // Ensure the timer does not keep the process alive
    if (this.flushTimer && typeof this.flushTimer === 'object' && 'unref' in this.flushTimer) {
      this.flushTimer.unref();
    }

    useLogger('telemetry').debug(`telemetry flush every ${this.flushIntervalS}s → ${endpoint}`);

    // Final flush on shutdown
    app.onStop(async () => {
      await this.stop();
    });
  }

  async stop(): Promise<void> {
    if (this.flushTimer) {
      clearInterval(this.flushTimer);
      this.flushTimer = undefined;
    }

    // Drain everything (including the current second) on shutdown
    if (this.collector) {
      await this.push(this.collector.drainAll(), this.collector.drainSpans()).catch((error) => {
        useLogger('telemetry').warn('Shutdown flush failed', { endpoint: this.target?.access.endpoint, error });
      });
    }

    // Only reset the process-global collector if it still points at ours, so a
    // second telemetry-enabled app in the same process is not torn down here.
    if (this.collector) {
      clearCollector(this.collector);
    }
    this.collector = undefined;
    this.middleware = undefined;
  }

  // -----------------------------------------------------------------------
  // Target
  // -----------------------------------------------------------------------

  private resolveTarget(config: InferConfig<typeof TelemetryConfig>): TelemetryTarget | undefined {
    let endpoint: string | undefined;
    let bearer: string | undefined;
    if (this.environment) {
      endpoint = this.options.endpoint;
    } else {
      if (!config.enabled) return undefined;
      // No public default endpoint: telemetry never ships data to a hardcoded
      // destination. If it is enabled but no endpoint is configured, stay inert.
      if (!config.endpoint) {
        useLogger('telemetry').warn(
          'telemetry is enabled but no endpoint is configured; telemetry will not start. ' +
            'Set telemetry.endpoint (config/env) or pass { endpoint } to telemetry().',
        );
        return undefined;
      }
      endpoint = config.endpoint;
      bearer = config.bearer;
    }
    if (!endpoint) return undefined;

    const identity = serviceIdentity(config.app);
    return {
      access: new CollectorAccess(endpoint, bearer),
      app: identity.serviceName,
      resourceAttributes: identity.resourceAttributes,
    };
  }

  // -----------------------------------------------------------------------
  // Flush
  // -----------------------------------------------------------------------

  private async flush(): Promise<void> {
    if (!this.collector) return;
    await this.push(this.collector.drain(), this.collector.drainSpans());
  }

  private async push(buckets: MetricsBucket[], spans: readonly TelemetrySpanRecord[]): Promise<void> {
    const target = this.target;
    if (!target || (buckets.length === 0 && spans.length === 0)) return;

    const credential = await target.access.credential();
    if (!credential.ok) return;

    const endpoint = target.access.endpoint;
    const opts = { app: target.app, bearer: credential.bearer, resourceAttributes: target.resourceAttributes };
    if (buckets.length > 0) target.access.observe(await sendMetrics(endpoint, buckets, opts));
    if (spans.length > 0) target.access.observe(await sendTraces(endpoint, spans, opts));
  }
}

/** Factory function for creating a TelemetryPlugin with optional configuration. */
export function telemetry(options: TelemetryOptions = {}): TelemetryPlugin {
  return new TelemetryPlugin(options);
}

/**
 * The telemetry plugin the environment asks for, or `undefined` when it names
 * no collector: `OTEL_EXPORTER_OTLP_ENDPOINT` turns it on and
 * `OTEL_SDK_DISABLED=true` turns it off. `application()` adds it when the
 * composition holds no `telemetry()` of its own.
 */
export function telemetryFromEnvironment(
  env: Readonly<Record<string, string | undefined>> = process.env,
): TelemetryPlugin | undefined {
  const endpoint = environmentEndpoint(env);
  return endpoint ? new TelemetryPlugin({ endpoint }, { fromEnvironment: true }) : undefined;
}
