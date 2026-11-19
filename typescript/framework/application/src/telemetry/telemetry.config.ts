import { Config, Default, Int, Optional, Sensitive } from '@putnami/runtime';
import type { TraceSampling } from './trace-sampling';

export interface TelemetryOptions {
  /** Enable or disable telemetry collection. Disabled by default (explicit opt-in). */
  enabled?: boolean;
  /**
   * OTLP/HTTP collector base URL (e.g. "https://collector:4318"). Metrics are
   * POSTed as OTLP/JSON to "<endpoint>/v1/metrics". Required when telemetry is
   * enabled — no default.
   */
  endpoint?: string;
  /** Bearer token for authenticating with the collector. */
  bearer?: string;
  /**
   * Seconds between two metric pushes. Default: OTEL_METRIC_EXPORT_INTERVAL
   * (milliseconds) when set, else 60.
   */
  metricIntervalS?: number;
  /**
   * Longest wait, in seconds, of a completed span before its batch is pushed;
   * a batch of 512 spans leaves at once. Default: OTEL_BSP_SCHEDULE_DELAY
   * (milliseconds) when set, else 30.
   */
  spanBatchDelayS?: number;
  /** Application identifier reported as the OTLP resource's service.name. Defaults to package name. */
  app?: string;
  /**
   * The trace budget: keep the first `budget` root traces, then keep a root
   * when `ratio` picks its trace ID, and at least `floorPerMinute` roots each
   * minute. A field left out takes its default (100, 0.01 and 1); each key of
   * the `telemetry.traces` config section overrides its field, live.
   */
  traces?: Partial<TraceSampling>;
}

export const TelemetryConfig = Config('telemetry', {
  // Opt-in by default: telemetry must be explicitly enabled before any
  // operational data leaves the process.
  enabled: Default(Boolean, false),
  // No public default — the collector base URL must be supplied explicitly when
  // telemetry is enabled, so data is never shipped to a hardcoded endpoint.
  endpoint: Optional(String),
  bearer: Sensitive(Optional(String)),
  metricIntervalS: Optional(Int),
  spanBatchDelayS: Optional(Int),
  app: Optional(String),
});
