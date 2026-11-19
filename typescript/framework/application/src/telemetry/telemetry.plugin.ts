/**
 * Telemetry Plugin — Main Orchestrator
 *
 * Lifecycle:
 *   warmup  → resolve the collector, init the collector, instrument HTTP outermost
 *   start   → start the metric and span timers
 *   stop    → final flush, clear timers, tear down collector
 *
 * The metric timer drains completed per-second buckets from the collector every
 * 60 seconds and POSTs them as OTLP/JSON to a standard OTLP/HTTP collector's
 * /v1/metrics. The span timer drains completed spans at most 30 seconds apart
 * to /v1/traces, and a batch of 512 spans leaves at once (telemetry.cadence.ts
 * says how to change both). Each push asks the registered telemetry token
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
import type { MetricDescription, MetricsBucket, TelemetrySpanRecord } from './telemetry.collector';
import {
  cadenceMs,
  DEFAULT_METRIC_INTERVAL_MS,
  DEFAULT_SPAN_BATCH_DELAY_MS,
  ENV_METRIC_EXPORT_INTERVAL,
  ENV_SPAN_BATCH_DELAY,
  spanBatchSize,
} from './telemetry.cadence';
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
  private metricTimer: ReturnType<typeof setInterval> | undefined;
  private spanTimer: ReturnType<typeof setInterval> | undefined;
  private options: TelemetryOptions;
  private target: TelemetryTarget | undefined;
  private middleware: HttpMiddleware | undefined;
  private readonly environment: boolean;

  /** Milliseconds between two metric pushes. */
  private metricIntervalMs = DEFAULT_METRIC_INTERVAL_MS;
  /** Longest wait, in milliseconds, of a completed span before its batch is pushed. */
  private spanBatchDelayMs = DEFAULT_SPAN_BATCH_DELAY_MS;

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
    this.metricIntervalMs = cadenceMs(config.metricIntervalS, ENV_METRIC_EXPORT_INTERVAL, DEFAULT_METRIC_INTERVAL_MS);
    this.spanBatchDelayMs = cadenceMs(config.spanBatchDelayS, ENV_SPAN_BATCH_DELAY, DEFAULT_SPAN_BATCH_DELAY_MS);

    this.collector = new TelemetryCollector({
      spanBatchSize: spanBatchSize(),
      onSpanBatchFull: () => this.background(this.pushSpans()),
    });
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

    this.metricTimer = this.every(this.metricIntervalMs, () => this.pushMetrics());
    this.spanTimer = this.every(this.spanBatchDelayMs, () => this.pushSpans());

    useLogger('telemetry').debug(
      `telemetry pushes metrics every ${this.metricIntervalMs}ms and spans at most ${this.spanBatchDelayMs}ms apart → ${endpoint}`,
    );

    // Final flush on shutdown
    app.onStop(async () => {
      await this.stop();
    });
  }

  async stop(): Promise<void> {
    clearInterval(this.metricTimer);
    clearInterval(this.spanTimer);
    this.metricTimer = undefined;
    this.spanTimer = undefined;

    // Drain everything (including the current second) on shutdown
    if (this.collector) {
      const collector = this.collector;
      await this.push(collector.drainAll(), collector.drainSpans(), collector.metricDescriptions()).catch((error) => {
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

  /** Run `task` every `ms`, on a timer that does not keep the process alive. */
  private every(ms: number, task: () => Promise<void>): ReturnType<typeof setInterval> {
    const timer = setInterval(() => this.background(task()), ms);
    if (typeof timer === 'object' && 'unref' in timer) timer.unref();
    return timer;
  }

  /** Let a push run on its own: a failure is logged, never thrown. */
  private background(push: Promise<void>): void {
    push.catch((error) => {
      useLogger('telemetry').warn('Flush failed', { endpoint: this.target?.access.endpoint, error });
    });
  }

  private async pushMetrics(): Promise<void> {
    if (!this.collector) return;
    // Everything the push renders is read now, before it awaits a credential.
    await this.push(this.collector.drain(), [], this.collector.metricDescriptions());
  }

  private async pushSpans(): Promise<void> {
    if (!this.collector) return;
    await this.push([], this.collector.drainSpans(), {});
  }

  private async push(
    buckets: MetricsBucket[],
    spans: readonly TelemetrySpanRecord[],
    descriptions: Readonly<Record<string, MetricDescription>>,
  ): Promise<void> {
    const target = this.target;
    if (!target || (buckets.length === 0 && spans.length === 0)) return;

    const credential = await target.access.credential();
    if (!credential.ok) return;

    const endpoint = target.access.endpoint;
    const opts = { app: target.app, bearer: credential.bearer, resourceAttributes: target.resourceAttributes };
    if (buckets.length > 0) target.access.observe(await sendMetrics(endpoint, buckets, { ...opts, descriptions }));
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
