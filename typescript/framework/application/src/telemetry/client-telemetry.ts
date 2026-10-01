import { runInContext, tryContext } from '@putnami/runtime';
import type { TelemetryAttributes } from './telemetry.collector';
import { getCollector } from './telemetry.utils';

const TRACEPARENT = /^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$/;
const ZERO_TRACE_ID = '00000000000000000000000000000000';
const ZERO_SPAN_ID = '0000000000000000';
const MAX_TRACESTATE_LENGTH = 512;
const MAX_BAGGAGE_LENGTH = 8192;

export interface TelemetryTraceContext {
  traceId: string;
  spanId: string;
  traceFlags: string;
  tracestate?: string;
  baggage?: string;
}

interface AmbientTelemetryContext extends Record<string, unknown> {
  __putnamiTrace?: TelemetryTraceContext;
}

export interface TelemetrySpanOptions {
  name: string;
  kind: 'server' | 'client';
  attributes?: TelemetryAttributes;
  /** Standard W3C headers on an inbound request. */
  remoteHeaders?: Headers;
}

export interface TelemetrySpanResult {
  attributes?: TelemetryAttributes;
  /** Stable local framework code. Untrusted remote messages are never accepted. */
  code?: string;
}

export interface ActiveTelemetrySpan {
  readonly context: TelemetryTraceContext;
  run<T>(action: () => T | Promise<T>): T | Promise<T>;
  inject(headers: Headers): void;
  finish(result?: TelemetrySpanResult): void;
}

/** Start a dependency-free W3C span backed by the active Putnami collector. */
export function startTelemetrySpan(options: TelemetrySpanOptions): ActiveTelemetrySpan {
  const ambient = tryContext<AmbientTelemetryContext>();
  const remote = options.remoteHeaders ? readRemoteContext(options.remoteHeaders) : undefined;
  const parent = remote ?? ambient?.__putnamiTrace;
  const context: TelemetryTraceContext = {
    traceId: parent?.traceId ?? randomHex(16),
    spanId: randomHex(8),
    traceFlags: parent?.traceFlags ?? '01',
    ...(parent?.tracestate ? { tracestate: parent.tracestate } : {}),
    ...(parent?.baggage ? { baggage: parent.baggage } : {}),
  };
  const started = Date.now();
  let finished = false;

  return {
    context,
    run: <T>(action: () => T | Promise<T>): T | Promise<T> => {
      const inherited = tryContext<AmbientTelemetryContext>();
      const derived = Object.assign(Object.create(inherited ?? null) as AmbientTelemetryContext, {
        __putnamiTrace: context,
        traceId: context.traceId,
      });
      return runInContext(derived, action);
    },
    inject: (headers) => injectContext(headers, context),
    finish: (result = {}) => {
      if (finished) return;
      finished = true;
      getCollector()?.addSpan({
        traceId: context.traceId,
        spanId: context.spanId,
        ...(parent?.spanId ? { parentSpanId: parent.spanId } : {}),
        name: options.name,
        kind: options.kind === 'server' ? 2 : 3,
        startTimeUnixNano: nanoFromMillis(started),
        endTimeUnixNano: nanoFromMillis(Date.now()),
        attributes: { ...(options.attributes ?? {}), ...(result.attributes ?? {}) },
        statusCode: result.code ? 2 : 1,
        ...(result.code ? { statusMessage: result.code } : {}),
      });
    },
  };
}

/** Attach a started span to an existing request context without replacing its DI identity. */
export function attachTelemetryContext(target: Record<string, unknown>, span: ActiveTelemetrySpan): void {
  target['__putnamiTrace'] = span.context;
  target['traceId'] = span.context.traceId;
}

function readRemoteContext(headers: Headers): TelemetryTraceContext | undefined {
  const match = TRACEPARENT.exec(headers.get('traceparent')?.toLowerCase() ?? '');
  if (!match || match[1] === ZERO_TRACE_ID || match[2] === ZERO_SPAN_ID) return undefined;
  const tracestate = boundedHeader(headers.get('tracestate'), MAX_TRACESTATE_LENGTH);
  const baggage = boundedHeader(headers.get('baggage'), MAX_BAGGAGE_LENGTH);
  return {
    traceId: match[1],
    spanId: match[2],
    traceFlags: match[3],
    ...(tracestate ? { tracestate } : {}),
    ...(baggage ? { baggage } : {}),
  };
}

function injectContext(headers: Headers, context: TelemetryTraceContext): void {
  headers.set('traceparent', `00-${context.traceId}-${context.spanId}-${context.traceFlags}`);
  if (context.tracestate) headers.set('tracestate', context.tracestate);
  else headers.delete('tracestate');
  if (context.baggage) headers.set('baggage', context.baggage);
  else headers.delete('baggage');
}

function boundedHeader(value: string | null, maximum: number): string | undefined {
  if (!value || value.length > maximum || /[\0\r\n]/.test(value)) return undefined;
  return value;
}

function randomHex(bytes: number): string {
  const value = crypto.getRandomValues(new Uint8Array(bytes));
  return Array.from(value, (byte) => byte.toString(16).padStart(2, '0')).join('');
}

function nanoFromMillis(value: number): string {
  return (BigInt(value) * 1_000_000n).toString();
}
