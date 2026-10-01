import type { ClientCachePolicy } from '@putnami/application';
import { incCounterWithAttributes } from '@putnami/application';
import { tryContext } from '@putnami/runtime';
import { getLogger } from '@putnami/utils';
import { CircuitOpenError } from './circuit-breaker';
import { CredentialRegistryClosedError } from './credential';
import {
  ClientCanceledError,
  ClientDeadlineError,
  ClientError,
  ClientFrameworkError,
  ClientRetryExhaustedError,
} from './errors';
import { encodeJsonBody, rawJsonNumberLexeme } from './json-codec';
import { resolveClientResilience } from './service-resilience';
import { isTransportFailure } from './transport-failure';
import type { ClientRequest, ClientResponse, Interceptor } from './transport.type';

const logger = getLogger('client');

/** Default per-operation entry bound, identical to the Go runtime's. */
export const DEFAULT_CACHE_MAX_ENTRIES = 1000;

/** Metric counting stored answers returned after a failed provider call. */
export const STALE_SERVED_METRIC = 'rpc.client.cache.stale_served';

/**
 * Contract capabilities this runtime implements. It equals the
 * `runtimeCapabilities` vocabulary of protocols/clientcontract's generated
 * manifest schema; a test pins the two together.
 */
export const CLIENT_RUNTIME_CAPABILITIES: readonly string[] = Object.freeze(['response-cache', 'sse-continuation']);

/**
 * Called by a generated module that needs a capability beyond the base
 * runtime. A runtime older than the capability has no such export, so the
 * generated module fails to load or type-check against it; a runtime that has
 * the export but not the capability throws when the module loads. Either way
 * the client never runs without the behavior its contract declares.
 */
export function requireClientRuntimeCapabilities(required: readonly string[]): void {
  for (const capability of required) {
    if (!CLIENT_RUNTIME_CAPABILITIES.includes(capability)) {
      throw new Error(
        `generated client requires runtime capability ${capability}, which @putnami/client does not implement`,
      );
    }
  }
}

/** Seams a test needs to drive the fresh and stale windows without waiting. */
export interface ServiceResponseCacheOptions {
  readonly now?: () => number;
}

interface CachedAnswer {
  readonly slot: string;
  readonly key: string;
  readonly response: ClientResponse;
  /** Each declared invalidation field the answer carries, with its canonical value. */
  readonly tags: ReadonlyMap<string, string>;
  readonly storedAt: number;
}

interface Flight {
  readonly key: string;
  promise: Promise<ClientResponse>;
  detached: boolean;
  waiters: number;
}

interface ResolvedCachePolicy {
  readonly freshMs: number;
  readonly staleMs: number;
  readonly maxEntries: number;
  readonly invalidationFields: readonly string[];
}

/**
 * One operation's entries on one service binding. A Map keeps insertion order,
 * so re-inserting on use makes its first key the least recently used.
 */
class OperationCache {
  readonly entries = new Map<string, CachedAnswer>();
  readonly flights = new Map<string, Flight>();

  constructor(readonly policy: ResolvedCachePolicy) {}

  lookup(slot: string, now: number): CachedAnswer | undefined {
    const entry = this.entries.get(slot);
    if (!entry) return undefined;
    if (now - entry.storedAt >= Math.max(this.policy.freshMs, this.policy.staleMs)) {
      this.entries.delete(slot);
      return undefined;
    }
    return entry;
  }

  touch(entry: CachedAnswer): void {
    this.entries.delete(entry.slot);
    this.entries.set(entry.slot, entry);
  }

  store(entry: CachedAnswer): void {
    this.entries.delete(entry.slot);
    this.entries.set(entry.slot, entry);
    while (this.entries.size > this.policy.maxEntries) {
      const oldest = this.entries.keys().next().value as string;
      this.entries.delete(oldest);
    }
  }
}

/**
 * The response cache an application's service registry owns for one service:
 * one bounded least-recently-used cache per operation and declared policy,
 * shared by every client bound from the registry, and ended with it.
 */
export class ServiceResponseCache {
  private readonly operations = new Map<string, OperationCache>();
  private readonly now: () => number;
  /** Aborts every shared call in flight when the registry ends. */
  private readonly lifetime = new AbortController();
  private disposed = false;

  constructor(options: ServiceResponseCacheOptions = {}) {
    this.now = options.now ?? Date.now;
  }

  /**
   * Answer one call from memory, or through the single upstream call in
   * flight for its key and identity. `call` runs the rest of the chain under
   * the signal that ends with the registry; `stale` decides whether its
   * failure may be masked by a stored answer.
   */
  async serve(
    operationId: string,
    declared: ClientCachePolicy,
    identity: string,
    key: string,
    caller: CacheCaller,
    call: (signal: AbortSignal) => Promise<ClientResponse>,
    stale: (error: unknown) => boolean,
    servedStale: (ageMs: number, error: unknown) => void,
  ): Promise<ClientResponse> {
    this.assertOpen();
    // A caller that already walked away or ran out of time starts nothing and
    // is handed nothing, stored or not.
    const ended = callerEnd(caller);
    if (ended === 'canceled') throw new ClientCanceledError('', operationId);
    if (ended === 'deadline') throw new ClientDeadlineError('', operationId);
    const policy = resolvePolicy(declared);
    const cache = this.operation(operationId, policy);
    const slot = `${identity}\u0000${key}`;
    const now = this.now();
    const entry = cache.lookup(slot, now);
    if (entry && now - entry.storedAt < policy.freshMs) {
      cache.touch(entry);
      return cloneResponse(entry.response);
    }
    let flight = cache.flights.get(slot);
    if (flight) flight.waiters++;
    else flight = this.fly(cache, slot, key, call);
    const outcome = await waitForFlight(flight.promise, caller);
    if ('response' in outcome) return cloneResponse(outcome.response);
    if ('end' in outcome) {
      // An explicit cancellation is always the caller's answer. A caller whose
      // own deadline passed while the provider hung is the outage the stale
      // window exists for: it takes the stored answer, and the shared call
      // keeps running for the callers still waiting on it.
      if (outcome.end === 'canceled') throw new ClientCanceledError('', operationId);
      const deadline = new ClientDeadlineError('', operationId);
      const answer = this.staleAnswer(cache, slot, policy);
      if (!answer) throw deadline;
      servedStale(answer.ageMs, deadline);
      return answer.response;
    }
    if (!stale(outcome.error)) throw outcome.error;
    const answer = this.staleAnswer(cache, slot, policy);
    if (!answer) throw outcome.error;
    servedStale(answer.ageMs, outcome.error);
    return answer.response;
  }

  /**
   * Drop every entry whose key starts with `keyPrefix`, for every identity,
   * and detach every matching call in flight so its answer is never stored.
   * Returns how many entries were dropped.
   */
  invalidate(keyPrefix: string): number {
    let dropped = 0;
    for (const cache of this.operations.values()) {
      for (const [slot, entry] of cache.entries) {
        if (entry.key.startsWith(keyPrefix)) {
          cache.entries.delete(slot);
          dropped++;
        }
      }
      for (const [slot, flight] of cache.flights) {
        if (flight.key.startsWith(keyPrefix)) {
          flight.detached = true;
          cache.flights.delete(slot);
        }
      }
    }
    return dropped;
  }

  /**
   * Drop every entry whose declared invalidation `field` carries `tag`, for
   * every identity and across the service's operations. A call in flight for
   * an operation that declares the field has no answer yet, so nothing says
   * that answer will not carry the value: it is detached, its answer is never
   * stored, and the next caller goes upstream. Returns how many entries were
   * dropped.
   */
  invalidateByField(field: string, tag: string): number {
    let dropped = 0;
    for (const cache of this.operations.values()) {
      if (!cache.policy.invalidationFields.includes(field)) continue;
      for (const [slot, entry] of cache.entries) {
        if (entry.tags.get(field) === tag) {
          cache.entries.delete(slot);
          dropped++;
        }
      }
      for (const flight of cache.flights.values()) flight.detached = true;
      cache.flights.clear();
    }
    return dropped;
  }

  /** How many callers joined a call already in flight, summed over every operation. */
  waiting(): number {
    let total = 0;
    for (const cache of this.operations.values()) for (const flight of cache.flights.values()) total += flight.waiters;
    return total;
  }

  /**
   * Drop every entry, abort every shared call in flight, and refuse every
   * later call: the callers waiting on an aborted call see the registry
   * closed. Idempotent.
   */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const cache of this.operations.values()) {
      cache.entries.clear();
      for (const flight of cache.flights.values()) flight.detached = true;
      cache.flights.clear();
    }
    this.operations.clear();
    this.lifetime.abort(new CredentialRegistryClosedError());
  }

  get isDisposed(): boolean {
    return this.disposed;
  }

  private operation(operationId: string, policy: ResolvedCachePolicy): OperationCache {
    // Field names hold no control character, so NUL separates them unambiguously.
    const name = `${operationId}\u0000${policy.freshMs}/${policy.staleMs}/${policy.maxEntries}\u0000${policy.invalidationFields.join('\u0000')}`;
    let cache = this.operations.get(name);
    if (!cache) {
      cache = new OperationCache(policy);
      this.operations.set(name, cache);
    }
    return cache;
  }

  private fly(
    cache: OperationCache,
    slot: string,
    key: string,
    call: (signal: AbortSignal) => Promise<ClientResponse>,
  ): Flight {
    const flight = { key, detached: false, waiters: 0 } as Flight;
    cache.flights.set(slot, flight);
    flight.promise = this.land(cache, slot, flight, call);
    // A flight nobody awaits any more must not surface as an unhandled rejection.
    flight.promise.catch(() => {});
    return flight;
  }

  private async land(
    cache: OperationCache,
    slot: string,
    flight: Flight,
    call: (signal: AbortSignal) => Promise<ClientResponse>,
  ): Promise<ClientResponse> {
    try {
      const response = await call(this.lifetime.signal);
      // The registry closed while the call was running: its answer does not
      // outlive the application that asked for it.
      if (this.disposed) throw new CredentialRegistryClosedError();
      if (!flight.detached)
        cache.store({
          slot,
          key: flight.key,
          response: cloneResponse(response),
          tags: responseTags(response.data, cache.policy.invalidationFields),
          storedAt: this.now(),
        });
      return response;
    } catch (error) {
      // Closing the registry aborted the call; every caller waiting on it
      // sees the registry closed, not the abort it caused.
      if (this.disposed) throw new CredentialRegistryClosedError();
      throw error;
    } finally {
      if (cache.flights.get(slot) === flight) cache.flights.delete(slot);
    }
  }

  /**
   * Read the slot's entry again, when the policy declares a stale window. An
   * invalidation that landed while the call was in flight removed it, and a
   * removed answer is never served, stale or not.
   */
  private staleAnswer(
    cache: OperationCache,
    slot: string,
    policy: ResolvedCachePolicy,
  ): { response: ClientResponse; ageMs: number } | undefined {
    if (policy.staleMs <= 0 || this.disposed) return undefined;
    const at = this.now();
    const stored = cache.lookup(slot, at);
    if (!stored || at - stored.storedAt >= policy.staleMs) return undefined;
    return { response: cloneResponse(stored.response), ageMs: at - stored.storedAt };
  }

  private assertOpen(): void {
    if (this.disposed) throw new CredentialRegistryClosedError();
  }
}

function resolvePolicy(declared: ClientCachePolicy): ResolvedCachePolicy {
  return {
    freshMs: declared.freshMs,
    staleMs: declared.staleMs ?? 0,
    maxEntries: declared.maxEntries ?? DEFAULT_CACHE_MAX_ENTRIES,
    invalidationFields: [...(declared.invalidationFields ?? [])],
  };
}

/** A value an invalidation by a response field compares: a string, an integer or a boolean. */
export type ResponseFieldValue = string | number | bigint | boolean;

/**
 * Render one invalidation value canonically (ADR 0007 of
 * protocols/clientcontract): a string as JSON.stringify quotes it, a boolean as
 * `true` or `false`, an integer — a safe integer number, a bigint, or a raw
 * JSON integer lexeme — as its decimal digits with no sign on zero. Every
 * other value, null and fractions included, has no tag. The Go runtime renders
 * the same bytes; protocols/clientcontract/fixtures/cache/invalidation.json
 * pins both.
 */
export function cacheFieldValue(value: unknown): string | undefined {
  switch (typeof value) {
    case 'string':
      return JSON.stringify(value);
    case 'boolean':
      return String(value);
    case 'bigint':
      return value.toString(10);
    case 'number':
      return Number.isSafeInteger(value) ? String(value) : undefined;
    default: {
      const lexeme = rawJsonNumberLexeme(value);
      return lexeme !== undefined && /^-?(0|[1-9]\d*)$/.test(lexeme) ? BigInt(lexeme).toString(10) : undefined;
    }
  }
}

/**
 * Read each declared invalidation field from a stored answer's decoded body.
 * A field the body does not carry, carries as null, or carries as anything but
 * a string, an integer or a boolean tags nothing; so does a body that is not a
 * JSON object.
 */
export function responseTags(data: unknown, fields: readonly string[]): ReadonlyMap<string, string> {
  const tags = new Map<string, string>();
  if (fields.length === 0 || typeof data !== 'object' || data === null || Array.isArray(data)) return tags;
  for (const field of fields) {
    if (!Object.hasOwn(data, field)) continue;
    const tag = cacheFieldValue((data as Record<string, unknown>)[field]);
    if (tag !== undefined) tags.set(field, tag);
  }
  return tags;
}

/**
 * Check an invalidation's field and render its value in the canonical form
 * stored answers are tagged with. A value no runtime can compare is refused,
 * never silently matched against nothing.
 */
export function invalidationTag(field: string, value: ResponseFieldValue): string {
  if (field === '' || field.startsWith(' ') || field.endsWith(' ') || [...field].some(isControlCharacter)) {
    throw new TypeError(`invalidation field ${JSON.stringify(field)} must name a top-level response property`);
  }
  const tag = typeof value === 'object' ? undefined : cacheFieldValue(value);
  if (tag === undefined) {
    throw new TypeError(
      `invalidation field ${JSON.stringify(field)} takes a string, a safe integer, a bigint or a boolean, not ${
        value === null ? 'null' : typeof value
      }${typeof value === 'number' ? ` ${String(value)}` : ''}`,
    );
  }
  return tag;
}

function isControlCharacter(char: string): boolean {
  const code = char.codePointAt(0) ?? 0;
  return code < 0x20 || code === 0x7f;
}

/** The response cache interceptor with its invalidation hooks. */
export type ResponseCacheInterceptor = Interceptor & {
  invalidateResponses(keyPrefix: string): number;
  invalidateResponsesByField(field: string, value: ResponseFieldValue): number;
};

/** Whether an interceptor carries the response cache's invalidation hook. */
export function isResponseCacheInterceptor(interceptor: Interceptor): interceptor is ResponseCacheInterceptor {
  return typeof (interceptor as Partial<ResponseCacheInterceptor>).invalidateResponses === 'function';
}

/**
 * Answer a generated unary call from its operation's declared response cache.
 * It runs first in the chain, so a fresh answer costs no deadline, credential,
 * breaker or retry, and a stale one masks their failure. Operations that
 * declare no cache go straight through.
 */
export function serviceResponseCacheInterceptor(
  serviceId: string,
  cache: ServiceResponseCache,
  endpoint = '',
): ResponseCacheInterceptor {
  const interceptor: Interceptor = async (request, next) => {
    const declared = request.clientOperation?.resilience?.cache;
    const operation = request.clientOperation;
    // A bypassed call neither reads, stores nor joins a call in flight, and no
    // stored answer masks its failure: it is the provider's current answer.
    if (!declared || !operation || operation.stream !== 'unary' || request.withoutResponseCache) return next(request);
    if (operation.idempotency.kind !== 'safe' && operation.idempotency.kind !== 'idempotent') return next(request);
    const operationId = request.operationId ?? `${request.method} ${request.path}`;
    const key = responseCacheKey(operationId, request);
    if (key === undefined) return next(request);
    const identity = JSON.stringify([endpoint, await forwardedIdentity()]);
    const ambient = tryContext<{ signal?: AbortSignal; deadlineAt?: number }>();
    const deadlines = [request.deadlineAt, ambient?.deadlineAt].filter(
      (deadline): deadline is number => deadline !== undefined && Number.isFinite(deadline),
    );
    const caller: CacheCaller = {
      signals: [request.signal, ambient?.signal].filter((signal): signal is AbortSignal => signal !== undefined),
      ...(deadlines.length ? { deadlineAt: Math.min(...deadlines) } : {}),
    };
    const resilience = resolveClientResilience(request);
    return cache.serve(
      operationId,
      declared,
      identity,
      key,
      caller,
      // The shared call answers every caller waiting on this key, so no single
      // caller's cancellation or deadline bounds it: the operation's declared
      // budget does, and the registry's end aborts it.
      (signal) => next({ ...request, signal, detachedFromCaller: true }),
      (error) => staleServable(error, request, resilience),
      (ageMs, error) => reportStaleServed(serviceId, request, ageMs, error),
    );
  };
  return Object.assign(interceptor, {
    invalidateResponses: (keyPrefix: string) => cache.invalidate(keyPrefix),
    invalidateResponsesByField: (field: string, value: ResponseFieldValue) =>
      cache.invalidateByField(field, invalidationTag(field, value)),
  });
}

/**
 * A failure a stored answer may mask: the provider could not be reached or
 * kept failing. A canceled caller, a request the provider refused and a
 * response that broke the contract all reach the caller.
 */
export function staleServable(
  error: unknown,
  request: ClientRequest,
  resilience: ReturnType<typeof resolveClientResilience>,
): boolean {
  if (error instanceof ClientCanceledError) return false;
  if (error instanceof CircuitOpenError || error instanceof ClientDeadlineError) return true;
  // Only a failure the transport's own network call raised is an outage: a
  // TypeError from any other code is a defect, and a stored answer must never
  // hide one.
  if (error instanceof ClientRetryExhaustedError) return isTransportFailure(error.lastError);
  if (isTransportFailure(error)) return true;
  if (error instanceof Error && error.name === 'TimeoutError') return true;
  if (error instanceof ClientFrameworkError) {
    const declared = request.clientOperation?.errors.find(
      (entry) => entry.status === error.status && entry.code === error.code,
    );
    if (declared?.retryable !== undefined) return declared.retryable;
    return resilience.retry.statuses.includes(error.status) || resilience.retry.codes.includes(error.code);
  }
  if (error instanceof ClientError && error.status > 0) return resilience.retry.statuses.includes(error.status);
  return false;
}

function reportStaleServed(serviceId: string, request: ClientRequest, ageMs: number, error: unknown): void {
  const attributes = {
    'rpc.system': 'putnami',
    'rpc.service': serviceId,
    'rpc.method': request.operationId ?? request.path,
    'network.protocol.name': request.clientOperation?.transports[0]?.protocol ?? 'rest-json',
  };
  incCounterWithAttributes(STALE_SERVED_METRIC, 1, attributes);
  logger.warn('served a stale cached response after a provider failure', {
    ...attributes,
    ageMs,
    'error.type': stableCode(error),
  });
}

function stableCode(error: unknown): string {
  const code = (error as { code?: unknown } | undefined)?.code;
  if (error instanceof ClientError && typeof code === 'string' && code) return code;
  return error instanceof Error ? error.name : 'unknown';
}

/**
 * The identity half of a cache slot: the SHA-256 of the forwarded user token
 * the call carries, or empty when it carries none. The token never enters the
 * cache.
 */
async function forwardedIdentity(): Promise<string> {
  const authorization = tryContext<{ __authorizationHeader?: string }>()?.__authorizationHeader?.trim();
  if (!authorization) return '';
  const bearer = /^bearer\s+(.+)$/i.exec(authorization);
  const token = (bearer?.[1] ?? authorization).trim();
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(token));
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('');
}

function cloneResponse(response: ClientResponse): ClientResponse {
  return {
    data: structuredClone(response.data),
    status: response.status,
    headers: new Headers(response.headers),
    // The stored bytes are the provider's; every caller gets its own copy.
    ...(response.successBody ? { successBody: new Uint8Array(response.successBody) } : {}),
  };
}

/**
 * The caller half of a call: what may end its wait before the shared call
 * answers. A signal aborted with a `TimeoutError` reason is a deadline, any
 * other abort is a cancellation.
 */
interface CacheCaller {
  readonly signals: readonly AbortSignal[];
  /** Absolute deadline in epoch milliseconds. */
  readonly deadlineAt?: number;
}

type CallerEnd = 'canceled' | 'deadline';

const MAX_TIMER_DELAY_MS = 2_147_483_647;

type FlightOutcome = { response: ClientResponse } | { error: unknown } | { end: CallerEnd };

function signalEnd(signal: AbortSignal): CallerEnd {
  return (signal.reason as { name?: unknown } | undefined)?.name === 'TimeoutError' ? 'deadline' : 'canceled';
}

/** How the caller's own budget stands right now, if it already ended. */
function callerEnd(caller: CacheCaller): CallerEnd | undefined {
  for (const signal of caller.signals) if (signal.aborted) return signalEnd(signal);
  if (caller.deadlineAt !== undefined && caller.deadlineAt <= Date.now()) return 'deadline';
  return undefined;
}

/** Wait for the shared call, or for the caller's own cancellation or deadline. */
function waitForFlight(promise: Promise<ClientResponse>, caller: CacheCaller): Promise<FlightOutcome> {
  return new Promise<FlightOutcome>((resolve) => {
    const cleanups: (() => void)[] = [];
    let settled = false;
    const settle = (outcome: FlightOutcome) => {
      if (settled) return;
      settled = true;
      for (const cleanup of cleanups) cleanup();
      resolve(outcome);
    };
    for (const signal of caller.signals) {
      const abort = () => settle({ end: signalEnd(signal) });
      signal.addEventListener('abort', abort, { once: true });
      cleanups.push(() => signal.removeEventListener('abort', abort));
    }
    const remaining = caller.deadlineAt === undefined ? Number.POSITIVE_INFINITY : caller.deadlineAt - Date.now();
    // A timer longer than the platform's 32-bit bound would fire at once, so a
    // deadline that far away is left to the operation's own budget.
    if (remaining <= MAX_TIMER_DELAY_MS) {
      const timer = setTimeout(() => settle({ end: 'deadline' }), Math.max(0, remaining));
      cleanups.push(() => clearTimeout(timer));
    }
    promise.then(
      (response) => settle({ response }),
      (error: unknown) => settle({ error }),
    );
  });
}

/**
 * Render the canonical key of one call (ADR 0007 of protocols/clientcontract),
 * or undefined when its body cannot be rendered — the call then bypasses the
 * cache rather than guess.
 */
export function responseCacheKey(operationId: string, request: ClientRequest): string | undefined {
  const body = canonicalRequestBody(request);
  if (body === null) return undefined;
  const headers: Record<string, string[]> = {};
  for (const [name, value] of request.headers) headers[name] = [value];
  const query: Record<string, readonly string[]> = {};
  for (const [name, value] of Object.entries(request.query ?? {})) {
    if (value === undefined) continue;
    query[name] = typeof value === 'string' ? [value] : value;
  }
  return canonicalCacheKey(operationId, request.clientOperation?.resilience?.cache?.keyFields, {
    keyHeader: request.clientOperation?.idempotency.keyHeader,
    path: request.params ?? {},
    query,
    headers,
    body,
  });
}

/**
 * The canonical body of a call: `undefined` when it has none, `null` when it
 * cannot be rendered — raw octets that are not bytes, or a JSON body its own
 * schema refuses.
 */
function canonicalRequestBody(request: ClientRequest): string | undefined | null {
  if (request.body === undefined) return undefined;
  if (request.requestMediaType) {
    const body = request.body;
    const bytes = body instanceof Uint8Array ? body : body instanceof ArrayBuffer ? new Uint8Array(body) : undefined;
    return bytes ? JSON.stringify(base64(bytes)) : null;
  }
  try {
    const wire = request.requestSchema
      ? encodeJsonBody(request.body, request.requestSchema, request.clientSchemas)
      : JSON.stringify(request.body);
    return canonicalizeJsonText(wire);
  } catch {
    return null;
  }
}

/** The runtime-independent inputs of a key; the shared vectors pin the rendering. */
export interface CacheKeyInputs {
  readonly keyHeader?: string;
  readonly path: Readonly<Record<string, string>>;
  readonly query: Readonly<Record<string, readonly string[]>>;
  readonly headers: Readonly<Record<string, readonly string[]>>;
  /** The canonical body, already rendered. */
  readonly body?: string;
}

/**
 * Render a key from its inputs. The Go runtime renders the same bytes for the
 * same inputs; protocols/clientcontract/fixtures/cache/keys.json pins both.
 */
// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: the default key and each declared section render in one pass the Go runtime mirrors
export function canonicalCacheKey(
  operationId: string,
  keyFields: readonly string[] | undefined,
  inputs: CacheKeyInputs,
): string {
  const parts: string[] = [];
  const add = (field: string, value: string | undefined) => {
    parts.push(value === undefined ? field : `${field}=${cacheKeyEscape(value)}`);
  };
  const keyHeader = inputs.keyHeader?.toLowerCase();
  const headers = new Map<string, string[]>();
  for (const [name, values] of Object.entries(inputs.headers)) {
    const lower = name.toLowerCase();
    if (lower === 'content-type' || (keyHeader !== undefined && lower === keyHeader)) continue;
    // A repeated field and its ", "-combined form are the same field (the
    // combination rule HTTP and Fetch apply), so each value is split at that
    // separator: Headers hands a repeated field over combined, http.Header
    // hands it over repeated, and both render one JSON array.
    headers.set(lower, [...(headers.get(lower) ?? []), ...values.flatMap((value) => value.split(', '))]);
  }
  if (!keyFields || keyFields.length === 0) {
    for (const name of sortByCodePoint(Object.keys(inputs.path)))
      add(`path.${name}`, JSON.stringify(inputs.path[name]));
    for (const name of sortByCodePoint(Object.keys(inputs.query)))
      add(`query.${name}`, canonicalStrings(inputs.query[name]));
    for (const name of sortByCodePoint([...headers.keys()])) add(`header.${name}`, canonicalStrings(headers.get(name)));
    if (inputs.body !== undefined) add('body', inputs.body);
    return `${operationId}?${parts.join('&')}`;
  }
  for (const field of keyFields) {
    const dot = field.indexOf('.');
    const section = field === 'body' ? 'body' : field.slice(0, Math.max(dot, 0));
    const name = field === 'body' ? '' : field.slice(dot + 1);
    switch (section) {
      case 'path':
        add(field, Object.hasOwn(inputs.path, name) ? JSON.stringify(inputs.path[name]) : undefined);
        break;
      case 'query':
        add(field, canonicalStrings(inputs.query[name]));
        break;
      case 'header':
        add(field, canonicalStrings(headers.get(name.toLowerCase())));
        break;
      case 'body':
        add(field, bodyField(inputs.body, name));
        break;
      default:
        add(field, undefined);
    }
  }
  return `${operationId}?${parts.join('&')}`;
}

function canonicalStrings(values: readonly string[] | undefined): string | undefined {
  if (!values || values.length === 0) return undefined;
  if (values.length === 1) return JSON.stringify(values[0]);
  return `[${values.map((value) => JSON.stringify(value)).join(',')}]`;
}

function bodyField(body: string | undefined, name: string): string | undefined {
  if (body === undefined) return undefined;
  if (name === '') return body;
  const members = objectMembers(body);
  return members?.get(name);
}

/**
 * Canonicalize JSON text: object keys sorted by code point, no insignificant
 * whitespace, number lexemes kept exactly as sent, strings escaped the way
 * JSON.stringify escapes them. Numbers are never parsed, so a wide integer
 * keeps every digit.
 */
export function canonicalizeJsonText(text: string): string {
  const parser = new CanonicalJson(text);
  const value = parser.value();
  parser.end();
  return value;
}

/** Top-level members of a canonical JSON object, each rendered canonically. */
function objectMembers(text: string): Map<string, string> | undefined {
  const parser = new CanonicalJson(text);
  const members = parser.members();
  if (members) parser.end();
  return members;
}

class CanonicalJson {
  private index = 0;

  constructor(private readonly text: string) {}

  value(): string {
    this.skip();
    const char = this.text[this.index];
    if (char === '{') {
      const members = this.members() as Map<string, string>;
      return `{${sortByCodePoint([...members.keys()])
        .map((name) => `${JSON.stringify(name)}:${members.get(name)}`)
        .join(',')}}`;
    }
    if (char === '[') {
      this.index++;
      const items: string[] = [];
      this.skip();
      if (this.text[this.index] === ']') {
        this.index++;
        return '[]';
      }
      for (;;) {
        items.push(this.value());
        this.skip();
        if (this.text[this.index] === ',') {
          this.index++;
          continue;
        }
        this.expect(']');
        return `[${items.join(',')}]`;
      }
    }
    if (char === '"') return JSON.stringify(this.string());
    for (const literal of ['true', 'false', 'null']) {
      if (this.text.startsWith(literal, this.index)) {
        this.index += literal.length;
        return literal;
      }
    }
    const number = /-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/y;
    number.lastIndex = this.index;
    const match = number.exec(this.text);
    if (!match) throw new SyntaxError('invalid JSON value');
    this.index += match[0].length;
    return match[0];
  }

  members(): Map<string, string> | undefined {
    this.skip();
    if (this.text[this.index] !== '{') return undefined;
    this.index++;
    const members = new Map<string, string>();
    this.skip();
    if (this.text[this.index] === '}') {
      this.index++;
      return members;
    }
    for (;;) {
      this.skip();
      const name = this.string();
      this.skip();
      this.expect(':');
      members.set(name, this.value());
      this.skip();
      if (this.text[this.index] === ',') {
        this.index++;
        continue;
      }
      this.expect('}');
      return members;
    }
  }

  end(): void {
    this.skip();
    if (this.index !== this.text.length) throw new SyntaxError('trailing JSON data');
  }

  private string(): string {
    if (this.text[this.index] !== '"') throw new SyntaxError('expected a JSON string');
    let end = this.index + 1;
    while (end < this.text.length && this.text[end] !== '"') end += this.text[end] === '\\' ? 2 : 1;
    const token = this.text.slice(this.index, end + 1);
    this.index = end + 1;
    return JSON.parse(token) as string;
  }

  private expect(char: string): void {
    if (this.text[this.index] !== char) throw new SyntaxError(`expected ${char}`);
    this.index++;
  }

  private skip(): void {
    while (this.index < this.text.length && ' \t\n\r'.includes(this.text[this.index] as string)) this.index++;
  }
}

/** Sort by Unicode code point, the order Go's byte-wise string sort yields. */
function sortByCodePoint(values: string[]): string[] {
  return values.sort((left, right) => {
    const a = [...left];
    const b = [...right];
    for (let index = 0; index < Math.min(a.length, b.length); index++) {
      const difference = (a[index]?.codePointAt(0) ?? 0) - (b[index]?.codePointAt(0) ?? 0);
      if (difference !== 0) return difference;
    }
    return a.length - b.length;
  });
}

/** Percent-encode every UTF-8 byte outside A-Z a-z 0-9 - . _ ~. */
export function cacheKeyEscape(value: string): string {
  let out = '';
  for (const byte of new TextEncoder().encode(value)) {
    const char = String.fromCharCode(byte);
    out += /[A-Za-z0-9\-._~]/.test(char) ? char : `%${byte.toString(16).toUpperCase().padStart(2, '0')}`;
  }
  return out;
}

function base64(bytes: Uint8Array): string {
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}
