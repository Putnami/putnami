/**
 * Telemetry Sender — OTLP/JSON metrics transport.
 *
 * Renders aggregated metric buckets into the canonical OTLP/JSON envelope (see
 * ./otlp and the telemetry protocol) and POSTs them to a standard OTLP/HTTP
 * collector's /v1/metrics. Fire-and-forget: failures are swallowed and never
 * affect application behaviour.
 */

import {
  marshalCanonical,
  metricsRequestFromBuckets,
  OTLP_CONTENT_TYPE,
  PATH_METRICS,
  PATH_TRACES,
  tracesRequestFromSpans,
} from './otlp';
import type { MetricsBucket, TelemetrySpanRecord } from './telemetry.collector';

const SEND_TIMEOUT_MS = 5000;

/** Result of an OTLP send. */
export interface OtlpSendResult {
  ok: boolean;
}

export interface MetricsSendOptions {
  /** Service name reported as the OTLP resource's service.name. */
  app: string;
  /** Optional service.version resource attribute. */
  serviceVersion?: string;
  /** Bearer token for the collector. */
  bearer?: string;
  /** Extra headers (e.g. tenant routing). */
  headers?: Record<string, string>;
}

type TracesSendOptions = MetricsSendOptions;

/** Build the OTLP/JSON metrics request body from per-second buckets. */
export function buildMetricsBody(buckets: MetricsBucket[], opts: MetricsSendOptions): string {
  const req = metricsRequestFromBuckets(buckets, { serviceName: opts.app, serviceVersion: opts.serviceVersion });
  return marshalCanonical(req);
}

/** Build the OTLP/JSON traces request body from completed framework spans. */
export function buildTracesBody(spans: readonly TelemetrySpanRecord[], opts: TracesSendOptions): string {
  return marshalCanonical(
    tracesRequestFromSpans(spans, { serviceName: opts.app, serviceVersion: opts.serviceVersion }),
  );
}

/** Join a collector base URL with a signal path (e.g. /v1/metrics). */
export function joinUrl(base: string, path: string): string {
  return base.replace(/\/+$/, '') + path;
}

/**
 * POST an OTLP/JSON body to a collector signal endpoint with a 5-second
 * timeout. Best-effort: transport errors and non-2xx responses resolve to
 * `{ ok: false }` and are otherwise ignored.
 */
export async function postOtlp(
  endpoint: string,
  path: string,
  body: string,
  opts: { bearer?: string; headers?: Record<string, string> } = {},
): Promise<OtlpSendResult> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), SEND_TIMEOUT_MS);
  try {
    const headers: Record<string, string> = { 'Content-Type': OTLP_CONTENT_TYPE, ...opts.headers };
    if (opts.bearer) {
      headers['Authorization'] = `Bearer ${opts.bearer}`;
    }
    const res = await fetch(joinUrl(endpoint, path), {
      method: 'POST',
      headers,
      body,
      signal: controller.signal,
    });
    return { ok: res.ok };
  } catch {
    return { ok: false };
  } finally {
    clearTimeout(timeout);
  }
}

/** Render and POST per-second metric buckets to the collector's /v1/metrics. */
export async function sendMetrics(
  endpoint: string,
  buckets: MetricsBucket[],
  opts: MetricsSendOptions,
): Promise<OtlpSendResult> {
  const body = buildMetricsBody(buckets, opts);
  return postOtlp(endpoint, PATH_METRICS, body, { bearer: opts.bearer, headers: opts.headers });
}

/** Render and POST completed spans to the collector's /v1/traces endpoint. */
export async function sendTraces(
  endpoint: string,
  spans: readonly TelemetrySpanRecord[],
  opts: TracesSendOptions,
): Promise<OtlpSendResult> {
  const body = buildTracesBody(spans, opts);
  return postOtlp(endpoint, PATH_TRACES, body, { bearer: opts.bearer, headers: opts.headers });
}
