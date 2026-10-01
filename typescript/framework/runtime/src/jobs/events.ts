/**
 * Putnami runtime event protocol — TypeScript emitter and validator.
 *
 * The wire format is owned by `go.putnami.dev/protocol/runtime`
 * (`protocols/runtime`): every job subprocess emits structured
 * JSONL events on stdout, one self-contained JSON object per line. This
 * module lets TypeScript jobs speak the same contract the Go extension
 * SDK emits, validated against the shared fixture corpus.
 *
 * Two versions exist. `v: 1` admits exactly the nine original event types;
 * `v: 2` adds one — `ready`, the typed readiness signal — and nothing else. A
 * stream speaks ONE of them: mixing versions in one stream is rejected, so the
 * version is chosen once (from the invoking CLI's advertisement) and stamped on
 * every line.
 */

/** Runtime event protocol version 1: the nine original event types. */
export const PROTOCOL_VERSION = 1;

/** Runtime event protocol version 2: version 1 plus the `ready` event. */
export const PROTOCOL_VERSION_2 = 2;

/** Highest protocol version this build can emit and validate. */
export const MAX_KNOWN_PROTOCOL_VERSION = PROTOCOL_VERSION_2;

/** Event type discriminators admitted by protocol version 1. */
export const EVENT_TYPES = [
  'log',
  'progress',
  'artifact',
  'diagnostic',
  'metric',
  'phase',
  'summary',
  'result',
  'meta',
] as const;

/** The typed readiness event, admitted only by protocol version 2. */
export const READY_EVENT_TYPE = 'ready';

/** Event type discriminators admitted by protocol version 2: v1's plus `ready`. */
export const EVENT_TYPES_V2 = [...EVENT_TYPES, READY_EVENT_TYPE] as const;

export type EventType = (typeof EVENT_TYPES_V2)[number];

/** Reports whether `v` is a protocol version this build understands. */
export function isKnownProtocolVersion(v: unknown): boolean {
  return v === PROTOCOL_VERSION || v === PROTOCOL_VERSION_2;
}

/**
 * Reserved environment variable an invoking CLI sets to advertise the highest
 * runtime event protocol version it accepts. Mirrors
 * `runtime.AcceptedVersionEnv` in `protocols/runtime/negotiation.go`.
 */
export const RUNTIME_EVENTS_ENV = 'PUTNAMI_RUNTIME_EVENTS';

/**
 * Resolves an advertised value into the protocol version an emitter must stamp
 * on every line. Total by construction, and identical to the Go resolution:
 * absent / blank / unparsable / lower ⇒ v1; higher than this build knows ⇒
 * clamped down, because an invoker that accepts a newer version accepts every
 * version below it.
 */
export function negotiatedRuntimeEventVersion(raw: string | undefined | null): number {
  if (raw === undefined || raw === null) return PROTOCOL_VERSION;
  const trimmed = raw.trim();
  if (!/^[+-]?\d+$/.test(trimmed)) return PROTOCOL_VERSION;
  // Overflow parity with Go: strconv.Atoi ERRORS outside int64 (→ v1, fail
  // closed), while parseInt would silently lose precision and clamp a
  // nonsense advertisement to the newest version. Compare in big-integer
  // space so every band behaves exactly as Go: outside int64 → v1; inside
  // int64 → the same lower-bound/clamp rules, precision-exact.
  const INT64_MAX = 9223372036854775807n;
  const INT64_MIN = -9223372036854775808n;
  const asBig = BigInt(trimmed);
  if (asBig > INT64_MAX || asBig < INT64_MIN) return PROTOCOL_VERSION;
  if (asBig < BigInt(PROTOCOL_VERSION)) return PROTOCOL_VERSION;
  if (asBig > BigInt(MAX_KNOWN_PROTOCOL_VERSION)) return MAX_KNOWN_PROTOCOL_VERSION;
  return Number(asBig);
}

/** Ready targets: WHAT became ready. Closed vocabulary, canonical order. */
export const READY_TARGETS = ['server', 'workload'] as const;
export type ReadyTarget = (typeof READY_TARGETS)[number];

/** Ready endpoint schemes. Closed vocabulary, canonical (sorted) order. */
export const READY_ENDPOINT_SCHEMES = ['grpc', 'http', 'https', 'tcp'] as const;
export type ReadyEndpointScheme = (typeof READY_ENDPOINT_SCHEMES)[number];

/** Highest addressable TCP port. */
const MAX_PORT = 65_535;

/**
 * One address a ready target accepts traffic on.
 *
 * The URL is DERIVED ({@link readyEndpointUrl}) rather than carried, so an
 * endpoint cannot ship a `url` that disagrees with its own members.
 */
export interface ReadyEndpoint {
  scheme: string;
  host: string;
  port: number;
  /** Base path served under, when it is not `/`. Starts with `/` when present. */
  path?: string;
}

/** The payload of a `ready` event, carried in the event's `data`. */
export interface ReadyData {
  target: string;
  /** Disambiguates several targets of one workload (`api`, `admin`). */
  name?: string;
  /** Canonically ordered, duplicate-free. Required for target `server`. */
  endpoints?: ReadyEndpoint[];
  /** Milliseconds from process start to readiness. */
  durationMs?: number;
}

/** Returns the address an endpoint denotes. Never travels on the wire. */
export function readyEndpointUrl(endpoint: ReadyEndpoint): string {
  let url = `${endpoint.scheme}://${endpoint.host}`;
  if (endpoint.port !== 0) url += `:${endpoint.port}`;
  if (endpoint.path && endpoint.path !== '/') url += endpoint.path;
  return url;
}

/**
 * The canonical total order on endpoints: scheme, then host, then port, then
 * path — compared by BYTE order, never locale order, so this and the Go
 * comparator produce the same sequence. `localeCompare` would not.
 */
export function compareReadyEndpoints(a: ReadyEndpoint, b: ReadyEndpoint): number {
  if (a.scheme !== b.scheme) return a.scheme < b.scheme ? -1 : 1;
  if (a.host !== b.host) return a.host < b.host ? -1 : 1;
  if (a.port !== b.port) return a.port < b.port ? -1 : 1;
  const aPath = a.path ?? '';
  const bPath = b.path ?? '';
  if (aPath !== bPath) return aPath < bPath ? -1 : 1;
  return 0;
}

/** Returns a copy of `endpoints` in canonical order. */
export function sortReadyEndpoints(endpoints: ReadyEndpoint[]): ReadyEndpoint[] {
  return [...endpoints].sort(compareReadyEndpoints);
}

/**
 * Reserved top-level member of a structured log record carrying a
 * {@link ReadyData} payload. Mirrors `runtime.ReadyLogKey`.
 *
 * A served workload's stdout is a LOG stream, not a runtime event stream, so
 * this is how a framework announces readiness: the extension that spawned it
 * recognizes the key and emits the typed `ready` event.
 */
export const READY_LOG_KEY = 'putnami.ready';

/**
 * Builds the value a workload attaches under {@link READY_LOG_KEY}, in the
 * exact member order the Go emitter writes so both languages produce identical
 * bytes for identical facts. Endpoints are canonicalized; empty optional
 * members are omitted, matching Go's `omitempty`.
 */
export function readyMarker(data: ReadyData): ReadyData {
  const marker: ReadyData = { target: data.target };
  if (data.name) marker.name = data.name;
  if (data.endpoints && data.endpoints.length > 0) {
    marker.endpoints = sortReadyEndpoints(data.endpoints).map((endpoint) => {
      const copy: ReadyEndpoint = { scheme: endpoint.scheme, host: endpoint.host, port: endpoint.port };
      if (endpoint.path) copy.path = endpoint.path;
      return copy;
    });
  }
  if (data.durationMs) marker.durationMs = data.durationMs;
  return marker;
}

/**
 * Extracts the readiness payload from a decoded structured log record,
 * returning null when there is no marker OR when the marker would produce an
 * invalid `ready` event. Failing closed costs this one signal; emitting an
 * invalid line would cost the consumer the whole stream.
 */
export function readyMarkerFromLogRecord(record: Record<string, unknown> | null | undefined): ReadyData | null {
  if (!record) return null;
  const raw = record[READY_LOG_KEY];
  if (raw === undefined || raw === null) return null;
  if (typeof raw !== 'object' || Array.isArray(raw)) return null;
  const candidate = raw as ReadyData;
  if (typeof candidate.target !== 'string') return null;
  // Shape-check the endpoint list BEFORE readyMarker touches it: the input is a
  // workload's untrusted log payload, and a `null` or scalar entry would make
  // the canonicalizing map throw rather than fail closed.
  if (candidate.endpoints !== undefined) {
    if (!Array.isArray(candidate.endpoints)) return null;
    for (const endpoint of candidate.endpoints) {
      if (typeof endpoint !== 'object' || endpoint === null || Array.isArray(endpoint)) return null;
    }
  }
  const normalized = readyMarker(candidate);
  if (hasEventErrors(validateReadyData(normalized))) return null;
  return normalized;
}

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';
export type PhaseAction = 'start' | 'end';
export type PhaseStatus = 'success' | 'failed' | 'skipped';
export type ResultStatus = 'OK' | 'FAILED' | 'SKIP';
export type DiagnosticSeverity = 'error' | 'warning' | 'info' | 'hint';

/** Source position attached to diagnostics. */
export interface SourceLocation {
  file?: string;
  line?: number;
  column?: number;
}

/** Optional error details on forwarded log events. */
export interface ErrorInfo {
  message?: string;
  stack?: string;
}

/** Structured error carried by a failed result. */
export interface JobError {
  message?: string;
  code?: string;
}

/**
 * The JSONL event envelope. Type-specific fields are flat at the top
 * level, matching `protocols/runtime/schemas/event.json`.
 */
export interface RuntimeEvent {
  v: number;
  type: EventType;
  time?: string;
  level?: string;
  message?: string;
  data?: unknown;
  context?: Record<string, unknown>;
  error?: ErrorInfo;
  current?: number;
  total?: number;
  severity?: DiagnosticSeverity;
  code?: string;
  location?: SourceLocation;
  name?: string;
  value?: number;
  unit?: string;
  action?: PhaseAction;
  status?: string;
  id?: string;
  kind?: string;
  path?: string;
  [extra: string]: unknown;
}

/** A structural problem found by {@link validateRuntimeEvent}. */
export interface EventDiagnostic {
  code: string;
  field: string;
  message: string;
  /** Defaults to 'error' when omitted. */
  severity?: 'error' | 'warning';
}

/** Returns true when any diagnostic is an error (warnings don't count). */
export function hasEventErrors(diags: EventDiagnostic[]): boolean {
  return diags.some((d) => d.severity !== 'warning');
}

const LOG_LEVELS = new Set(['debug', 'info', 'warn', 'error']);
const SEVERITIES = new Set(['error', 'warning', 'info', 'hint']);
const PHASE_ACTIONS = new Set(['start', 'end']);
const EVENT_TYPE_SET = new Set<string>(EVENT_TYPES);
const EVENT_TYPE_SET_V2 = new Set<string>(EVENT_TYPES_V2);
const READY_TARGET_SET = new Set<string>(READY_TARGETS);
const READY_SCHEME_SET = new Set<string>(READY_ENDPOINT_SCHEMES);

/**
 * The event vocabulary a protocol version admits. An unknown version is checked
 * against v1 so a bad `v` reports one diagnostic instead of a version
 * diagnostic plus a spurious unknown-type one — same rule as the Go validator.
 */
function eventTypesForVersion(v: unknown): Set<string> {
  return v === PROTOCOL_VERSION_2 ? EVENT_TYPE_SET_V2 : EVENT_TYPE_SET;
}

/**
 * Validates a readiness payload, mirroring the Go `validateReadyData`. The
 * rules exist so a consumer can ACT on the event: a server claim carries an
 * address, the address is well-formed, and the endpoint list is in one
 * canonical order so two runs produce the same bytes.
 */
export function validateReadyData(data: ReadyData): EventDiagnostic[] {
  const diags: EventDiagnostic[] = [];
  const err = (code: string, field: string, message: string) => {
    diags.push({ code, field, message });
  };

  // Untrusted input: a wire payload can carry ANY shape here. A non-array
  // endpoints member must become a diagnostic, never a .forEach crash — the
  // validator's whole job is to return findings on malformed events.
  const rawEndpoints = data.endpoints ?? [];
  const endpoints = Array.isArray(rawEndpoints) ? rawEndpoints : [];
  if (!Array.isArray(rawEndpoints)) {
    err('invalid-type', 'data.endpoints', 'endpoints must be an array');
  }
  if (!data.target) {
    err('required-field', 'data.target', 'ready event requires a target');
  } else if (!READY_TARGET_SET.has(data.target)) {
    err('invalid-enum', 'data.target', `invalid ready target "${data.target}"`);
  } else if (data.target === 'server' && endpoints.length === 0) {
    err('required-field', 'data.endpoints', 'ready event with target "server" requires at least one endpoint');
  }

  if (data.durationMs !== undefined && data.durationMs < 0) {
    err('invalid-value', 'data.durationMs', `durationMs must be non-negative, got ${data.durationMs}`);
  }

  endpoints.forEach((endpoint, i) => {
    const field = (member: string) => `data.endpoints[${i}].${member}`;
    if (!endpoint.scheme) {
      err('required-field', field('scheme'), 'endpoint requires a scheme');
    } else if (!READY_SCHEME_SET.has(endpoint.scheme)) {
      err('invalid-enum', field('scheme'), `invalid endpoint scheme "${endpoint.scheme}"`);
    }
    if (!endpoint.host) {
      err('required-field', field('host'), 'endpoint requires a host');
    }
    if (typeof endpoint.port !== 'number' || endpoint.port < 1 || endpoint.port > MAX_PORT) {
      err('invalid-value', field('port'), `endpoint port ${endpoint.port} out of range [1,${MAX_PORT}]`);
    }
    if (endpoint.path && !endpoint.path.startsWith('/')) {
      err('invalid-value', field('path'), `endpoint path "${endpoint.path}" must start with "/"`);
    }
  });

  // Canonical order is a determinism rule, not a style preference: a digest
  // over the event must not depend on the order listeners happened to bind in.
  for (let i = 1; i < endpoints.length; i++) {
    const order = compareReadyEndpoints(endpoints[i - 1], endpoints[i]);
    if (order === 0) {
      err('duplicate-endpoint', `data.endpoints[${i}]`, `endpoint ${readyEndpointUrl(endpoints[i])} is listed twice`);
    } else if (order > 0) {
      err(
        'non-canonical-order',
        `data.endpoints[${i}]`,
        `endpoints must be in canonical order; ${readyEndpointUrl(endpoints[i])} precedes ${readyEndpointUrl(endpoints[i - 1])}`,
      );
    }
  }

  return diags;
}

/**
 * Validates the structural invariants of a single parsed event,
 * mirroring the Go protocol's `ValidateEvent`.
 */
export function validateRuntimeEvent(event: RuntimeEvent): EventDiagnostic[] {
  const diags: EventDiagnostic[] = [];
  const err = (code: string, field: string, message: string) => {
    diags.push({ code, field, message });
  };

  if (!isKnownProtocolVersion(event.v)) {
    err(
      'invalid-version',
      'v',
      `expected protocol version ${PROTOCOL_VERSION} or ${PROTOCOL_VERSION_2}, got ${event.v}`,
    );
  }
  // The admitted vocabulary is version-scoped: a `ready` event stamped v1 is an
  // unknown type, which is what keeps v2 additive rather than a retroactive
  // widening of v1.
  if (!eventTypesForVersion(event.v).has(event.type)) {
    err('invalid-event-type', 'type', `unknown event type "${event.type}"`);
    return diags;
  }

  switch (event.type) {
    case 'log':
      if (!event.level) err('required-field', 'level', 'log event requires level');
      else if (!LOG_LEVELS.has(event.level)) err('invalid-enum', 'level', `invalid log level "${event.level}"`);
      if (!event.message) err('required-field', 'message', 'log event requires message');
      break;
    case 'progress':
      if (event.current === undefined) err('required-field', 'current', 'progress event requires current');
      if (event.total === undefined) err('required-field', 'total', 'progress event requires total');
      break;
    case 'diagnostic':
      if (!event.severity) err('required-field', 'severity', 'diagnostic event requires severity');
      else if (!SEVERITIES.has(event.severity)) err('invalid-enum', 'severity', `invalid severity "${event.severity}"`);
      if (!event.message) err('required-field', 'message', 'diagnostic event requires message');
      break;
    case 'metric':
      if (!event.name) err('required-field', 'name', 'metric event requires name');
      if (typeof event.value !== 'number') err('required-field', 'value', 'metric event requires a numeric value');
      break;
    case 'phase':
      if (!event.name) err('required-field', 'name', 'phase event requires name');
      if (!event.action) err('required-field', 'action', 'phase event requires action');
      else if (!PHASE_ACTIONS.has(event.action))
        err('invalid-enum', 'action', `invalid phase action "${event.action}"`);
      break;
    case 'artifact':
      if (!event.id) err('required-field', 'id', 'artifact event requires id');
      if (!event.path) err('required-field', 'path', 'artifact event requires path');
      break;
    case 'summary':
      if (!event.message) err('required-field', 'message', 'summary event requires message');
      break;
    case READY_EVENT_TYPE: {
      const data = event.data;
      if (data === undefined || data === null) {
        err('required-field', 'data', 'ready event requires data');
        break;
      }
      if (typeof data !== 'object' || Array.isArray(data)) {
        err('invalid-ready-data', 'data', 'ready event data is not a readiness payload');
        break;
      }
      diags.push(...validateReadyData(data as ReadyData));
      break;
    }
    default:
      break;
  }

  return diags;
}

/**
 * Validates a full event sequence, mirroring the Go protocol's
 * `ValidateEventStream`: every event must be valid and the stream must
 * contain exactly one result event, as its last event.
 */
export function validateRuntimeEventStream(events: RuntimeEvent[]): EventDiagnostic[] {
  const diags: EventDiagnostic[] = [];
  let resultCount = 0;
  let lastResultIdx = -1;
  const versions = new Set<unknown>();

  events.forEach((event, i) => {
    for (const d of validateRuntimeEvent(event)) {
      diags.push({ ...d, field: `event[${i}].${d.field}` });
    }
    versions.add(event.v);
    if (event.type === 'result') {
      resultCount++;
      lastResultIdx = i;
    }
  });

  // One subprocess emits one contract. A stream that changes version mid-flight
  // cannot be read as either version, so it is rejected rather than interpreted
  // per line — this is what forbids "readiness at v2 alongside v1 logs".
  if (versions.size > 1) {
    const listed = [...versions]
      .map((v) => Number(v))
      .sort((a, b) => a - b)
      .join(', ');
    diags.push({
      code: 'mixed-protocol-version',
      field: '',
      message: `event stream mixes protocol versions ${listed}`,
    });
  }

  if (resultCount === 0) {
    diags.push({ code: 'missing-result', field: '', message: 'event stream must contain exactly one result event' });
  } else if (resultCount > 1) {
    diags.push({
      code: 'multiple-results',
      field: '',
      message: `event stream contains ${resultCount} result events, expected exactly one`,
    });
  }
  if (resultCount === 1 && lastResultIdx !== events.length - 1) {
    diags.push({
      code: 'result-not-last',
      field: '',
      message: 'result event should be the last event in the stream',
      severity: 'warning',
    });
  }

  return diags;
}

/** Parses one JSONL line into a runtime event, or null when it is not one. */
export function parseRuntimeEvent(line: string): RuntimeEvent | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return null;
  }
  const event = parsed as RuntimeEvent;
  if (!isKnownProtocolVersion(event.v) || typeof event.type !== 'string') {
    return null;
  }
  return event;
}

/** Receives serialized JSONL lines (newline included). */
export type EventSink = (line: string) => void;

function defaultSink(line: string): void {
  process.stdout.write(line);
}

function now(): string {
  return new Date().toISOString().replace(/\.(\d{3})\d*Z$/, '.$1Z');
}

/** Reads the invoking CLI's protocol advertisement, defaulting to v1. */
function advertisedRuntimeEventVersion(): number {
  const raw = typeof process !== 'undefined' && process.env ? process.env[RUNTIME_EVENTS_ENV] : undefined;
  return negotiatedRuntimeEventVersion(raw);
}

/**
 * Emits runtime protocol events as JSONL, mirroring the Go extension
 * SDK emitter (`tooling/extension-sdk/jsonl`) method for method so jobs
 * behave identically regardless of implementation language.
 *
 * The stream's protocol version is fixed at construction — from the invoking
 * CLI's advertisement by default — and stamped on EVERY line, because the
 * protocol rejects a stream that mixes versions. No advertisement means v1, so
 * an older consumer receives the byte-identical stream it always parsed.
 */
export class JobEventEmitter {
  private readonly sink: EventSink;
  private readonly version: number;

  constructor(sink: EventSink = defaultSink, version: number = advertisedRuntimeEventVersion()) {
    this.sink = sink;
    this.version = isKnownProtocolVersion(version) ? version : PROTOCOL_VERSION;
  }

  /** The protocol version this emitter stamps on every event. */
  get protocolVersion(): number {
    return this.version;
  }

  /** Reports whether the negotiated stream admits the `ready` event. */
  get supportsReady(): boolean {
    return this.version >= PROTOCOL_VERSION_2;
  }

  private emit(event: RuntimeEvent): void {
    // The version is the STREAM's, never the individual event's: overwriting
    // here is what makes "one stream, one contract" true by construction.
    event.v = this.version;
    if (!event.time) event.time = now();
    this.sink(`${JSON.stringify(event)}\n`);
  }

  /** Emits a meta event identifying the extension and job. */
  meta(extension: string, job: string): void {
    this.emit({
      v: PROTOCOL_VERSION,
      type: 'meta',
      level: 'info',
      message: `Starting ${job}`,
      data: { extension, job },
    });
  }

  /** Emits a log event. */
  log(level: LogLevel, message: string, context?: Record<string, unknown>, error?: ErrorInfo): void {
    const event: RuntimeEvent = { v: PROTOCOL_VERSION, type: 'log', level, message };
    if (context && Object.keys(context).length > 0) event.context = context;
    if (error) event.error = error;
    this.emit(event);
  }

  info(message: string): void {
    this.log('info', message);
  }

  warn(message: string): void {
    this.log('warn', message);
  }

  error(message: string): void {
    this.log('error', message);
  }

  debug(message: string): void {
    this.log('debug', message);
  }

  /** Emits the start of a named phase. */
  phaseStart(name: string): void {
    this.emit({ v: PROTOCOL_VERSION, type: 'phase', name, action: 'start' });
  }

  /** Emits the end of a named phase. */
  phaseEnd(name: string, status: PhaseStatus): void {
    this.emit({ v: PROTOCOL_VERSION, type: 'phase', name, action: 'end', status });
  }

  /** Emits a progress event. */
  progress(current: number, total: number, message?: string): void {
    const event: RuntimeEvent = { v: PROTOCOL_VERSION, type: 'progress', current, total };
    if (message) event.message = message;
    this.emit(event);
  }

  /** Emits a diagnostic event with optional location and code. */
  diagnostic(severity: DiagnosticSeverity, message: string, location?: SourceLocation, code?: string): void {
    const event: RuntimeEvent = { v: PROTOCOL_VERSION, type: 'diagnostic', severity, message };
    if (location?.file) event.location = location;
    if (code) event.code = code;
    this.emit(event);
  }

  /**
   * Emits a metric event. The protocol requires numeric values; the
   * value is dropped with a debug log when it is not a finite number.
   */
  metric(name: string, value: number, unit?: string): void {
    if (!Number.isFinite(value)) {
      this.debug(`dropping metric ${name}: non-numeric value`);
      return;
    }
    const event: RuntimeEvent = { v: PROTOCOL_VERSION, type: 'metric', name, value };
    if (unit) event.unit = unit;
    this.emit(event);
  }

  /** Emits an artifact event, with optional extra top-level fields. */
  artifact(id: string, name: string, kind: string, path: string, extra?: Record<string, unknown>): void {
    const event: RuntimeEvent = { v: PROTOCOL_VERSION, type: 'artifact', id, name, kind, path };
    this.emitWithExtra(event, extra);
  }

  /** Emits a summary event, with optional extra top-level fields. */
  summary(message: string, extra?: Record<string, unknown>): void {
    const event: RuntimeEvent = { v: PROTOCOL_VERSION, type: 'summary', message };
    this.emitWithExtra(event, extra);
  }

  /**
   * Emits the typed readiness event and reports whether it was written.
   *
   * On a stream that was not negotiated up to v2 it writes NOTHING and returns
   * false: there is no valid v1 spelling of readiness, and downgrading would
   * either emit an unknown type or mix versions in one stream. The member order
   * matches the Go emitter's exactly — `v`, `type`, `time`, `data`, with `time`
   * set here rather than appended by {@link emit} — so both languages produce
   * identical bytes for identical facts.
   */
  ready(data: ReadyData): boolean {
    if (!this.supportsReady) return false;
    this.emit({ v: PROTOCOL_VERSION_2, type: READY_EVENT_TYPE, time: now(), data: readyMarker(data) });
    return true;
  }

  /** Emits the final result event. Exactly one per job execution. */
  result(status: ResultStatus, data?: Record<string, unknown>, error?: JobError): void {
    const payload: Record<string, unknown> = { status };
    if (data) payload['data'] = data;
    if (error) payload['error'] = error;
    this.emit({
      v: PROTOCOL_VERSION,
      type: 'result',
      level: 'info',
      message: `Job ${status}`,
      data: payload,
    });
  }

  private emitWithExtra(event: RuntimeEvent, extra?: Record<string, unknown>): void {
    if (extra) {
      for (const [key, value] of Object.entries(extra)) {
        if (!(key in event)) event[key] = value;
      }
    }
    this.emit(event);
  }
}
