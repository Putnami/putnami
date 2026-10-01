/**
 * Telemetry Plugin — Main Orchestrator
 *
 * Lifecycle:
 *   warmup  → ensure HttpPlugin, register middleware, init collector
 *   start   → start periodic flush timer
 *   stop    → final flush, clear timer, tear down collector
 *
 * The flush timer drains completed per-second buckets from the collector,
 * renders them as OTLP/JSON, and POSTs them to a standard OTLP/HTTP collector's
 * /v1/metrics. Logs are opt-in via OtlpLogSink on the application logger.
 */

import { useConfig, useLogger } from '@putnami/runtime';
import type { Module, Plugin } from '../application';
import { HttpPlugin } from '../http/http.plugin';
import { TelemetryCollector } from './telemetry.collector';
import { TelemetryConfig, type TelemetryOptions } from './telemetry.config';
import { TelemetryMiddleware } from './telemetry.middleware';
import { sendMetrics, sendTraces } from './telemetry.sender';
import { clearCollector, setCollector } from './telemetry.utils';

/** Orchestrates telemetry collection, periodic flushing, and shutdown draining. */
export class TelemetryPlugin implements Plugin {
  private collector: TelemetryCollector | undefined;
  private flushTimer: ReturnType<typeof setInterval> | undefined;
  private options: TelemetryOptions;

  /** Runtime flush interval — may be updated by the server. */
  private flushIntervalS = 30;

  constructor(options: TelemetryOptions = {}) {
    this.options = options;
  }

  // -----------------------------------------------------------------------
  // Lifecycle
  // -----------------------------------------------------------------------

  async warmup(app: Module): Promise<void> {
    const config = useConfig(TelemetryConfig, { confInit: this.options });

    if (!config.enabled) return;

    // No public default endpoint: telemetry never ships data to a hardcoded
    // destination. If it is enabled but no endpoint is configured, stay inert.
    if (!config.endpoint) {
      useLogger('telemetry').warn(
        'telemetry is enabled but no endpoint is configured; telemetry will not start. ' +
          'Set telemetry.endpoint (config/env) or pass { endpoint } to telemetry().',
      );
      return;
    }

    this.flushIntervalS = config.flushIntervalS;

    this.collector = new TelemetryCollector();
    setCollector(this.collector);

    // Register HTTP metrics middleware
    const httpPlugin = await app.ensurePlugin(HttpPlugin);
    httpPlugin.use(TelemetryMiddleware(this.collector));
  }

  async start(app: Module): Promise<void> {
    if (!this.collector) return;

    const config = useConfig(TelemetryConfig, { confInit: this.options });
    const logger = useLogger('telemetry');

    // The collector is only created in warmup when an endpoint is configured,
    // so this is a defensive narrowing rather than an expected branch.
    const endpoint = config.endpoint;
    if (!endpoint) return;

    // Periodic flush
    this.flushTimer = setInterval(() => {
      this.flush(endpoint, config.app, config.bearer).catch((error) => {
        useLogger('telemetry').warn('Flush failed', { endpoint, error });
      });
    }, this.flushIntervalS * 1000);

    // Ensure the timer does not keep the process alive
    if (this.flushTimer && typeof this.flushTimer === 'object' && 'unref' in this.flushTimer) {
      this.flushTimer.unref();
    }

    logger.debug(`telemetry flush every ${this.flushIntervalS}s → ${endpoint}`);

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
      const config = useConfig(TelemetryConfig, { confInit: this.options });
      const endpoint = config.endpoint;
      const buckets = this.collector.drainAll();
      const spans = this.collector.drainSpans();
      if (endpoint && buckets.length > 0) {
        await sendMetrics(endpoint, buckets, { app: config.app ?? 'unknown', bearer: config.bearer }).catch((error) => {
          useLogger('telemetry').warn('Shutdown flush failed', { endpoint, error });
        });
      }
      if (endpoint && spans.length > 0) {
        await sendTraces(endpoint, spans, { app: config.app ?? 'unknown', bearer: config.bearer }).catch((error) => {
          useLogger('telemetry').warn('Shutdown trace flush failed', { endpoint, error });
        });
      }
    }

    // Only reset the process-global collector if it still points at ours, so a
    // second telemetry-enabled app in the same process is not torn down here.
    if (this.collector) {
      clearCollector(this.collector);
    }
    this.collector = undefined;
  }

  // -----------------------------------------------------------------------
  // Flush
  // -----------------------------------------------------------------------

  private async flush(endpoint: string, app: string | undefined, bearer: string | undefined): Promise<void> {
    if (!this.collector) return;

    const buckets = this.collector.drain();
    const spans = this.collector.drainSpans();
    if (buckets.length === 0 && spans.length === 0) return;

    if (buckets.length > 0) await sendMetrics(endpoint, buckets, { app: app ?? 'unknown', bearer });
    if (spans.length > 0) await sendTraces(endpoint, spans, { app: app ?? 'unknown', bearer });
  }
}

/** Factory function for creating a TelemetryPlugin with optional configuration. */
export function telemetry(options: TelemetryOptions = {}): TelemetryPlugin {
  return new TelemetryPlugin(options);
}
