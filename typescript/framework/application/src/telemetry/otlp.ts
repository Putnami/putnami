/**
 * OTLP/JSON renderer — the TypeScript side of the telemetry protocol.
 *
 * This module builds the same OTLP/JSON envelopes as the Go reference renderer
 * (go.putnami.dev/protocol/telemetry) and serializes them byte-identically.
 * That equivalence is what lets a workload point the Go and TypeScript runtimes
 * at one collector and get identical bytes; it is pinned by
 * `cross-language.test.ts` against the protocol's golden digests.
 *
 * The rules that make output match Go's encoding/json (see the protocol's
 * otlp.go):
 *   - 64-bit integers (timestamps, counts, int values) are decimal STRINGS,
 *   - enums (temporality, span kind, status code, severity) are INTEGERS,
 *   - trace/span ids are lowercase hex strings,
 *   - attributes are sorted by key,
 *   - each builder returns an object literal whose keys are in the canonical
 *     field order, with omitted optional fields set to `undefined`. `JSON.stringify`
 *     preserves the insertion order of (non-integer) string keys, drops
 *     `undefined`, and does not HTML-escape, so the bytes match Go.
 *
 * Do not reorder the keys in the builders below without updating otlp.go and the
 * equivalence goldens in lockstep.
 */

import { createHash } from 'node:crypto';
import type { LogEntry, LogLevel } from '@putnami/runtime';

// A rendered OTLP node. Builders return plain objects whose key order is the
// canonical wire order.
type OtlpNode = Record<string, unknown>;

// Canonical signal paths (mirrors the protocol's PathMetrics/Traces/Logs).
export const PATH_METRICS = '/v1/metrics';
export const PATH_TRACES = '/v1/traces';
export const PATH_LOGS = '/v1/logs';
export const OTLP_CONTENT_TYPE = 'application/json';

// Resource attribute keys (mirror the protocol's Attr* constants).
const ATTR_SERVICE_NAME = 'service.name';
const ATTR_SERVICE_VERSION = 'service.version';
const ATTR_PUTNAMI_FRAMEWORK = 'putnami.framework';
const FRAMEWORK_TYPESCRIPT = 'typescript';

// Aggregation temporality (OTLP enum).
export const TEMPORALITY_DELTA = 1;

// Read a known field off an opaque OTLP node without tripping
// noPropertyAccessFromIndexSignature (used only by the comparators).
function nameOf(n: OtlpNode): string {
  return (n as unknown as { name: string }).name;
}
function keyOf(n: OtlpNode): string {
  return (n as unknown as { key: string }).key;
}

// ---------------------------------------------------------------------------
// AnyValue + attributes
// ---------------------------------------------------------------------------

export const stringVal = (s: string): OtlpNode => ({ stringValue: s });
const boolVal = (b: boolean): OtlpNode => ({ boolValue: b });
export const intVal = (n: number | bigint): OtlpNode => ({ intValue: String(n) });
const doubleVal = (n: number): OtlpNode => ({ doubleValue: n });

export const attr = (key: string, value: OtlpNode): OtlpNode => ({ key, value });

/** Build a sorted attribute list from a string map (mirrors AttrsFromStrings). */
export function attrsFromStrings(m: Record<string, string>): OtlpNode[] {
  return Object.keys(m)
    .sort()
    .map((k) => attr(k, stringVal(m[k])));
}

/** Sort an attribute list by key (mirrors SortAttrs). */
export function sortAttrs(attrs: OtlpNode[]): OtlpNode[] {
  return [...attrs].sort((a, b) => {
    const ak = keyOf(a);
    const bk = keyOf(b);
    return ak < bk ? -1 : ak > bk ? 1 : 0;
  });
}

function resource(attrs: OtlpNode[]): OtlpNode {
  return attrs.length ? { attributes: attrs } : {};
}

/** Build a Resource from a string map, dropping empty values (mirrors ResourceFromStrings). */
export function resourceFromStrings(m: Record<string, string>): OtlpNode {
  const filtered: Record<string, string> = {};
  for (const k of Object.keys(m)) {
    if (m[k]) filtered[k] = m[k];
  }
  return resource(attrsFromStrings(filtered));
}

export function scope(name?: string, version?: string): OtlpNode {
  return { name: name || undefined, version: version || undefined };
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

interface NumberDP {
  attributes?: OtlpNode[];
  startTimeUnixNano?: string;
  timeUnixNano?: string;
  asInt?: string;
  asDouble?: number;
}

export function numberDataPoint(dp: NumberDP): OtlpNode {
  return {
    attributes: dp.attributes?.length ? dp.attributes : undefined,
    startTimeUnixNano: dp.startTimeUnixNano || undefined,
    timeUnixNano: dp.timeUnixNano || undefined,
    asInt: dp.asInt,
    asDouble: dp.asDouble,
  };
}

interface HistogramDP {
  attributes?: OtlpNode[];
  startTimeUnixNano?: string;
  timeUnixNano?: string;
  count: string;
  sum?: number;
  bucketCounts?: string[];
  explicitBounds?: number[];
  min?: number;
  max?: number;
}

export function histogramDataPoint(dp: HistogramDP): OtlpNode {
  return {
    attributes: dp.attributes?.length ? dp.attributes : undefined,
    startTimeUnixNano: dp.startTimeUnixNano || undefined,
    timeUnixNano: dp.timeUnixNano || undefined,
    count: dp.count,
    sum: dp.sum,
    bucketCounts: dp.bucketCounts?.length ? dp.bucketCounts : undefined,
    explicitBounds: dp.explicitBounds?.length ? dp.explicitBounds : undefined,
    min: dp.min,
    max: dp.max,
  };
}

export function sum(dataPoints: OtlpNode[], temporality: number, isMonotonic: boolean): OtlpNode {
  return {
    dataPoints: dataPoints.length ? dataPoints : undefined,
    aggregationTemporality: temporality || undefined,
    isMonotonic: isMonotonic || undefined,
  };
}

export function gauge(dataPoints: OtlpNode[]): OtlpNode {
  return { dataPoints: dataPoints.length ? dataPoints : undefined };
}

export function histogram(dataPoints: OtlpNode[], temporality: number): OtlpNode {
  return {
    dataPoints: dataPoints.length ? dataPoints : undefined,
    aggregationTemporality: temporality || undefined,
  };
}

interface MetricInit {
  name: string;
  description?: string;
  unit?: string;
  sum?: OtlpNode;
  gauge?: OtlpNode;
  histogram?: OtlpNode;
}

export function metric(m: MetricInit): OtlpNode {
  return {
    name: m.name,
    description: m.description || undefined,
    unit: m.unit || undefined,
    sum: m.sum,
    gauge: m.gauge,
    histogram: m.histogram,
  };
}

export function scopeMetrics(scopeNode: OtlpNode, metrics: OtlpNode[]): OtlpNode {
  return { scope: scopeNode, metrics: metrics.length ? metrics : undefined };
}

export function resourceMetrics(resourceNode: OtlpNode, scopeMetricsArr: OtlpNode[]): OtlpNode {
  return { resource: resourceNode, scopeMetrics: scopeMetricsArr.length ? scopeMetricsArr : undefined };
}

export function metricsRequest(resourceMetricsArr: OtlpNode[]): OtlpNode {
  return { resourceMetrics: resourceMetricsArr.length ? resourceMetricsArr : undefined };
}

// ---------------------------------------------------------------------------
// Traces
// ---------------------------------------------------------------------------

export function spanStatus(code: number, message?: string): OtlpNode {
  return { code: code || undefined, message: message || undefined };
}

interface SpanInit {
  traceId: string;
  spanId: string;
  parentSpanId?: string;
  name: string;
  kind?: number;
  startTimeUnixNano?: string;
  endTimeUnixNano?: string;
  attributes?: OtlpNode[];
  status?: OtlpNode;
}

export function span(s: SpanInit): OtlpNode {
  return {
    traceId: s.traceId,
    spanId: s.spanId,
    parentSpanId: s.parentSpanId || undefined,
    name: s.name,
    kind: s.kind || undefined,
    startTimeUnixNano: s.startTimeUnixNano || undefined,
    endTimeUnixNano: s.endTimeUnixNano || undefined,
    attributes: s.attributes?.length ? s.attributes : undefined,
    status: s.status,
  };
}

export function scopeSpans(scopeNode: OtlpNode, spans: OtlpNode[]): OtlpNode {
  return { scope: scopeNode, spans: spans.length ? spans : undefined };
}

export function resourceSpans(resourceNode: OtlpNode, scopeSpansArr: OtlpNode[]): OtlpNode {
  return { resource: resourceNode, scopeSpans: scopeSpansArr.length ? scopeSpansArr : undefined };
}

export function tracesRequest(resourceSpansArr: OtlpNode[]): OtlpNode {
  return { resourceSpans: resourceSpansArr.length ? resourceSpansArr : undefined };
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

interface LogRecordInit {
  timeUnixNano?: string;
  observedTimeUnixNano?: string;
  severityNumber?: number;
  severityText?: string;
  body?: OtlpNode;
  attributes?: OtlpNode[];
  traceId?: string;
  spanId?: string;
}

export function logRecord(r: LogRecordInit): OtlpNode {
  return {
    timeUnixNano: r.timeUnixNano || undefined,
    observedTimeUnixNano: r.observedTimeUnixNano || undefined,
    severityNumber: r.severityNumber || undefined,
    severityText: r.severityText || undefined,
    body: r.body,
    attributes: r.attributes?.length ? r.attributes : undefined,
    traceId: r.traceId || undefined,
    spanId: r.spanId || undefined,
  };
}

export function scopeLogs(scopeNode: OtlpNode, logRecords: OtlpNode[]): OtlpNode {
  return { scope: scopeNode, logRecords: logRecords.length ? logRecords : undefined };
}

export function resourceLogs(resourceNode: OtlpNode, scopeLogsArr: OtlpNode[]): OtlpNode {
  return { resource: resourceNode, scopeLogs: scopeLogsArr.length ? scopeLogsArr : undefined };
}

export function logsRequest(resourceLogsArr: OtlpNode[]): OtlpNode {
  return { resourceLogs: resourceLogsArr.length ? resourceLogsArr : undefined };
}

// ---------------------------------------------------------------------------
// Serialization
// ---------------------------------------------------------------------------

/**
 * Serialize a built envelope to canonical OTLP/JSON. `JSON.stringify` preserves
 * the insertion order of (non-integer) string keys, drops `undefined`, and does
 * not HTML-escape, so this matches Go's MarshalCanonical byte-for-byte when the
 * builders above are used.
 */
export function marshalCanonical(value: unknown): string {
  return JSON.stringify(value);
}

/** Lowercase hex SHA-256 of a canonical envelope (mirrors Digest). */
export function digest(s: string): string {
  return createHash('sha256').update(s, 'utf8').digest('hex');
}

// ---------------------------------------------------------------------------
// Encoding helpers
// ---------------------------------------------------------------------------

/** Unix nanoseconds (as a decimal string) from whole unix seconds. */
function nanoFromUnixSeconds(sec: number): string {
  return (BigInt(Math.trunc(sec)) * 1_000_000_000n).toString();
}

/** Unix nanoseconds (as a decimal string) from unix milliseconds. */
export function nanoFromMillis(ms: number): string {
  return (BigInt(Math.trunc(ms)) * 1_000_000n).toString();
}

const SEVERITY: Record<LogLevel, { num: number; text: string }> = {
  debug: { num: 5, text: 'debug' },
  info: { num: 9, text: 'info' },
  warn: { num: 13, text: 'warn' },
  error: { num: 17, text: 'error' },
};

const HEX32 = /^[0-9a-f]{32}$/;
const HEX16 = /^[0-9a-f]{16}$/;

function logAnyValue(v: unknown): OtlpNode {
  switch (typeof v) {
    case 'string':
      return stringVal(v);
    case 'boolean':
      return boolVal(v);
    case 'number':
      return Number.isInteger(v) ? intVal(v) : doubleVal(v);
    case 'bigint':
      return intVal(v);
    case 'object':
      // Arrays and objects reaching here (an array, or a plain object at the
      // flatten depth limit) are JSON-encoded so they survive as structured
      // text instead of the "[object Object]" String() default.
      return v === null ? stringVal('null') : stringVal(safeJsonString(v));
    default:
      return stringVal(String(v));
  }
}

/** JSON-encode a log value without letting hostile or cyclic data break a telemetry flush. */
function safeJsonString(v: object): string {
  // Keep only the active traversal path so a shared object used by two sibling
  // fields still renders twice; only an actual cycle becomes [Circular].
  const ancestors: object[] = [];
  try {
    return (
      JSON.stringify(v, function (this: object, _key: string, value: unknown): unknown {
        if (typeof value === 'bigint') return value.toString();
        if (value !== null && typeof value === 'object') {
          while (ancestors.length > 0 && ancestors[ancestors.length - 1] !== this) {
            ancestors.pop();
          }
          if (ancestors.includes(value)) return '[Circular]';
          ancestors.push(value);
        }
        return value;
      }) ?? '[Unserializable]'
    );
  } catch {
    return '[Unserializable]';
  }
}

/** Max depth to flatten nested plain objects into dotted attribute keys. */
const MAX_ATTR_FLATTEN_DEPTH = 2;

/** True for objects with the default (or null) prototype — not arrays/class instances. */
function isPlainObject(v: unknown): v is Record<string, unknown> {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) {
    return false;
  }
  const proto = Object.getPrototypeOf(v);
  return proto === Object.prototype || proto === null;
}

/**
 * Flatten a nested plain-object attribute value into dotted keys
 * (`{ http: { method: 'GET' } }` → `http.method=GET`), bounded to
 * {@link MAX_ATTR_FLATTEN_DEPTH}. Arrays and deeper objects are emitted as a
 * single JSON-encoded attribute via {@link logAnyValue}. Mirrors the Go
 * telemetry renderer's appendFlatAttr so both runtimes emit identical attrs.
 */
function pushFlatAttr(attrs: OtlpNode[], key: string, v: unknown, depth: number): void {
  if (isPlainObject(v) && depth < MAX_ATTR_FLATTEN_DEPTH) {
    for (const [sk, sv] of Object.entries(v)) {
      pushFlatAttr(attrs, `${key}.${sk}`, sv, depth + 1);
    }
    return;
  }
  attrs.push(attr(key, logAnyValue(v)));
}

// ---------------------------------------------------------------------------
// High-level renderers (the runtime's actual data → OTLP)
// ---------------------------------------------------------------------------

/** A one-second metric bucket, matching TelemetryCollector's MetricsBucket. */
export interface RenderBucket {
  ts: number;
  counters: Record<string, number>;
  gauges: Record<string, number>;
  histograms: Record<string, { count: number; sum: number; min: number; max: number }>;
  counterSeries?: Array<{
    name: string;
    attributes: Readonly<Record<string, string | number | boolean>>;
    value: number;
  }>;
  histogramSeries?: Array<{
    name: string;
    attributes: Readonly<Record<string, string | number | boolean>>;
    aggregate: { count: number; sum: number; min: number; max: number };
  }>;
}

interface MetricsRenderOptions {
  serviceName: string;
  serviceVersion?: string;
  scopeName?: string;
  scopeVersion?: string;
}

/** Build the Putnami resource (framework marker + service identity). */
function putnamiResource(opts: { serviceName: string; serviceVersion?: string }): OtlpNode {
  return resourceFromStrings({
    [ATTR_PUTNAMI_FRAMEWORK]: FRAMEWORK_TYPESCRIPT,
    [ATTR_SERVICE_NAME]: opts.serviceName,
    [ATTR_SERVICE_VERSION]: opts.serviceVersion ?? '',
  });
}

function pushDP(m: Map<string, OtlpNode[]>, name: string, dp: OtlpNode): void {
  const arr = m.get(name);
  if (arr) arr.push(dp);
  else m.set(name, [dp]);
}

function byName(a: OtlpNode, z: OtlpNode): number {
  const an = nameOf(a);
  const zn = nameOf(z);
  return an < zn ? -1 : an > zn ? 1 : 0;
}

function counterDataPoint(value: number, start: string, end: string, attributes?: OtlpNode[]): OtlpNode {
  return Number.isInteger(value)
    ? numberDataPoint({ attributes, startTimeUnixNano: start, timeUnixNano: end, asInt: String(value) })
    : numberDataPoint({ attributes, startTimeUnixNano: start, timeUnixNano: end, asDouble: value });
}

function telemetryAttributes(attributes: Readonly<Record<string, string | number | boolean>>): OtlpNode[] {
  return Object.entries(attributes)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([key, value]) =>
      attr(
        key,
        typeof value === 'string'
          ? stringVal(value)
          : typeof value === 'boolean'
            ? boolVal(value)
            : Number.isInteger(value)
              ? intVal(value)
              : doubleVal(value),
      ),
    );
}

/** Accumulate one bucket's counters, gauges, and histograms into per-name data-point maps. */
function accumulateBucket(
  b: RenderBucket,
  counterDPs: Map<string, OtlpNode[]>,
  gaugeDPs: Map<string, OtlpNode[]>,
  histDPs: Map<string, OtlpNode[]>,
): void {
  const start = nanoFromUnixSeconds(b.ts);
  const end = nanoFromUnixSeconds(b.ts + 1);
  for (const [name, value] of Object.entries(b.counters)) {
    pushDP(counterDPs, name, counterDataPoint(value, start, end));
  }
  for (const [name, value] of Object.entries(b.gauges)) {
    pushDP(gaugeDPs, name, numberDataPoint({ timeUnixNano: end, asDouble: value }));
  }
  for (const [name, h] of Object.entries(b.histograms)) {
    pushDP(
      histDPs,
      name,
      histogramDataPoint({
        startTimeUnixNano: start,
        timeUnixNano: end,
        count: String(h.count),
        sum: h.sum,
        bucketCounts: [String(h.count)],
        min: h.min,
        max: h.max,
      }),
    );
  }
  for (const series of b.counterSeries ?? []) {
    pushDP(counterDPs, series.name, counterDataPoint(series.value, start, end, telemetryAttributes(series.attributes)));
  }
  for (const series of b.histogramSeries ?? []) {
    const h = series.aggregate;
    pushDP(
      histDPs,
      series.name,
      histogramDataPoint({
        attributes: telemetryAttributes(series.attributes),
        startTimeUnixNano: start,
        timeUnixNano: end,
        count: String(h.count),
        sum: h.sum,
        bucketCounts: [String(h.count)],
        min: h.min,
        max: h.max,
      }),
    );
  }
}

/**
 * Render per-second metric buckets into an OTLP metrics request. Counters and
 * histograms are DELTA (each bucket is one second's delta); gauges are
 * point-in-time. Metrics are emitted in name order with one data point per
 * bucket, so a name observed across N buckets yields N timestamped points.
 */
export function metricsRequestFromBuckets(buckets: RenderBucket[], opts: MetricsRenderOptions): OtlpNode {
  const counterDPs = new Map<string, OtlpNode[]>();
  const gaugeDPs = new Map<string, OtlpNode[]>();
  const histDPs = new Map<string, OtlpNode[]>();

  for (const b of [...buckets].sort((a, z) => a.ts - z.ts)) {
    accumulateBucket(b, counterDPs, gaugeDPs, histDPs);
  }

  const metrics: OtlpNode[] = [];
  for (const [name, dps] of counterDPs) {
    metrics.push(metric({ name, sum: sum(dps, TEMPORALITY_DELTA, true) }));
  }
  for (const [name, dps] of gaugeDPs) {
    metrics.push(metric({ name, gauge: gauge(dps) }));
  }
  for (const [name, dps] of histDPs) {
    metrics.push(metric({ name, histogram: histogram(dps, TEMPORALITY_DELTA) }));
  }
  metrics.sort(byName);

  if (metrics.length === 0) return metricsRequest([]);

  const sc = scope(opts.scopeName ?? 'putnami', opts.scopeVersion);
  return metricsRequest([resourceMetrics(putnamiResource(opts), [scopeMetrics(sc, metrics)])]);
}

export interface TraceRenderRecord {
  readonly traceId: string;
  readonly spanId: string;
  readonly parentSpanId?: string;
  readonly name: string;
  readonly kind: number;
  readonly startTimeUnixNano: string;
  readonly endTimeUnixNano: string;
  readonly attributes: Readonly<Record<string, string | number | boolean>>;
  readonly statusCode: number;
  readonly statusMessage?: string;
}

interface TracesRenderOptions {
  serviceName: string;
  serviceVersion?: string;
  scopeName?: string;
  scopeVersion?: string;
}

/** Render completed framework spans into a deterministic OTLP traces request. */
export function tracesRequestFromSpans(spans: readonly TraceRenderRecord[], opts: TracesRenderOptions): OtlpNode {
  if (spans.length === 0) return tracesRequest([]);
  const rendered = spans.map((record) =>
    span({
      traceId: record.traceId,
      spanId: record.spanId,
      parentSpanId: record.parentSpanId,
      name: record.name,
      kind: record.kind,
      startTimeUnixNano: record.startTimeUnixNano,
      endTimeUnixNano: record.endTimeUnixNano,
      attributes: telemetryAttributes(record.attributes),
      status: spanStatus(record.statusCode, record.statusMessage),
    }),
  );
  const instrumentation = scope(opts.scopeName ?? 'putnami', opts.scopeVersion);
  return tracesRequest([resourceSpans(putnamiResource(opts), [scopeSpans(instrumentation, rendered)])]);
}

interface LogsRenderOptions {
  serviceName: string;
  serviceVersion?: string;
  scopeName?: string;
  scopeVersion?: string;
}

const LOG_RESERVED_KEYS = new Set([
  'severity',
  'message',
  'timestamp',
  'logger',
  'traceId',
  'logging.googleapis.com/trace',
  'error',
  'data',
]);

/** Gather a log entry's logger name, context/data fields, and error fields into attributes. */
function entryAttrs(e: LogEntry): OtlpNode[] {
  const attrs: OtlpNode[] = [];
  const fields: Record<string, unknown> = {};

  // Match JsonSink's top-level field rules: context arrives first, then a
  // single object data parameter replaces same-named context fields.
  if (e.context) {
    for (const [key, value] of Object.entries(e.context)) {
      if (!LOG_RESERVED_KEYS.has(key)) fields[key] = value;
    }
  }
  if (e.data && e.data.length > 0) {
    const data = e.data[0];
    if (e.data.length === 1 && typeof data === 'object' && data !== null && !(data instanceof Error)) {
      for (const [key, value] of Object.entries(data as Record<string, unknown>)) {
        if (!LOG_RESERVED_KEYS.has(key)) fields[key] = value;
      }
    } else {
      fields['data'] = e.data;
    }
  }

  if (e.logger) attrs.push(attr('logger.name', stringVal(e.logger)));
  for (const [key, value] of Object.entries(fields)) pushFlatAttr(attrs, key, value, 0);
  if (e.error?.name) attrs.push(attr('error.type', stringVal(e.error.name)));
  if (e.error?.message) attrs.push(attr('error.message', stringVal(e.error.message)));
  return attrs;
}

function entryToRecord(e: LogEntry): OtlpNode {
  const sev = SEVERITY[e.level];
  const attrs = entryAttrs(e);
  return logRecord({
    timeUnixNano: nanoFromMillis(Date.parse(e.timestamp)),
    severityNumber: sev.num,
    severityText: sev.text,
    body: stringVal(e.message),
    attributes: attrs.length ? sortAttrs(attrs) : undefined,
    traceId: e.traceId && HEX32.test(e.traceId) ? e.traceId : undefined,
  });
}

/** Render framework log entries into an OTLP logs request. */
export function logsRequestFromEntries(entries: LogEntry[], opts: LogsRenderOptions): OtlpNode {
  if (entries.length === 0) return logsRequest([]);
  const records = entries.map(entryToRecord);
  const sc = scope(opts.scopeName ?? 'putnami', opts.scopeVersion);
  return logsRequest([resourceLogs(putnamiResource(opts), [scopeLogs(sc, records)])]);
}

export { HEX16, HEX32 };
