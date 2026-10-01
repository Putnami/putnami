import { Config, Default, Int, Optional, Sensitive } from '@putnami/runtime';

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
  /** How often (in seconds) to flush aggregated metrics. Default: 30. */
  flushIntervalS?: number;
  /** Application identifier reported as the OTLP resource's service.name. Defaults to package name. */
  app?: string;
}

export const TelemetryConfig = Config('telemetry', {
  // Opt-in by default: telemetry must be explicitly enabled before any
  // operational data leaves the process.
  enabled: Default(Boolean, false),
  // No public default — the collector base URL must be supplied explicitly when
  // telemetry is enabled, so data is never shipped to a hardcoded endpoint.
  endpoint: Optional(String),
  bearer: Sensitive(Optional(String)),
  flushIntervalS: Default(Int, 30),
  app: Optional(String),
});
