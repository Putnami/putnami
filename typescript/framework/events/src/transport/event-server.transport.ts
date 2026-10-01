import { tryContext } from '@putnami/runtime';
import {
  EVENT_FRAME_TYPES,
  EVENT_SERVER_DEFAULT_ENDPOINTS,
  EVENT_SERVER_ERROR_CODES,
  MANAGED_PUBLISH_CONTRACT_VERSION,
  PUTNAMI_EVENTS_PROTOCOL,
  isManagedPublishFrame,
  isManagedPublishReservedAttribute,
  type EventProtocol,
  type ManagedPublishFrame,
  type PublishRef,
} from '../protocol';
import type { HandlerDefinition } from '../handler/handler';
import type { Message, PublishOptions } from '../topic/message';
import type { Envelope, Transport } from './transport';

const DEFAULT_MAX_ATTEMPTS = 3;
const DEFAULT_BASE_DELAY_MS = 100;
const DEFAULT_MAX_DELAY_MS = 2000;
const DEFAULT_REQUEST_TIMEOUT_MS = 10_000;
const DEFAULT_MAX_REQUEST_BYTES = 1_048_576;
const MAX_ERROR_RESPONSE_BYTES = 65_536;

/** Stable config selector used by managed workload documents. */
export const EVENT_SERVER_TRANSPORT_KIND = 'eventserver' as const;

/** Input passed to the provider-owned credential adapter before any HTTP request. */
export interface EventServerTokenRequest {
  /** Exact configured token audience. The framework never normalizes it. */
  audience: string;
  /** Aborts when the caller cancels or the publish deadline expires. */
  signal: AbortSignal;
}

/**
 * Provider-neutral credential seam for Event Server publication.
 *
 * Google metadata and SDK calls deliberately live in the hosting adapter, not
 * in `@putnami/events`. The returned token is used only as an Authorization
 * header and is never retained on a public error.
 */
export type EventServerTokenSource = (request: EventServerTokenRequest) => Promise<string>;

/** Retry tuning for the bounded Event Server publish loop. */
export interface EventServerRetryConfig {
  /** Total HTTP attempts, including the first. Default: 3; maximum: 10. */
  maxAttempts?: number;
  /** Initial exponential-backoff delay. Default: 100ms. */
  baseDelayMs?: number;
  /** Maximum jittered delay. Default: 2s. */
  maxDelayMs?: number;
}

export interface EventServerTransportConfig {
  /** Managed admission/config compatibility version. Must be 1. */
  contractVersion: 1 | number;
  /** Event Server origin. Publication always uses the canonical `/events/publish` path. */
  endpoint: string;
  /** Exact workload ID-token audience passed to `tokenSource`. */
  audience: string;
  /** Wire protocol compatibility gate. Must be `putnami.events.v1`. */
  protocol: EventProtocol | string;
  /** Provider-owned token acquisition hook. */
  tokenSource: EventServerTokenSource;
  /** Optional local preflight hints. They are never sent as caller authority. */
  workspaceId?: string;
  environment?: string;
  workload?: string;
  topologyGenerationId?: string;
  /** Overall deadline applied when the caller does not provide an earlier one. Default: 10s. */
  requestTimeoutMs?: number;
  /** Maximum encoded request-frame bytes. Default: 1 MiB. */
  maxRequestBytes?: number;
  retry?: EventServerRetryConfig;
  /** @internal Deterministic HTTP seam for tests and non-standard runtimes. */
  fetch?: typeof globalThis.fetch;
  /** @internal Deterministic jitter seam for tests. */
  random?: () => number;
  /** @internal Deterministic wait seam for tests. */
  sleep?: (delayMs: number, signal: AbortSignal) => Promise<void>;
}

/** Managed publish body: the existing v1 publish frame under stricter workload rules. */
export type ManagedEventPublishFrame = ManagedPublishFrame;
export type EventServerPublishRef = PublishRef;

export type EventServerPublishOutcome = 'permanent' | 'retryable' | 'ambiguous';

interface PublishErrorDetails {
  status?: number;
  code?: string;
  retryAfterMs?: number;
  attempts?: number;
}

/** Base class for stable publisher outcome classification. */
export class EventServerPublishError extends Error {
  readonly outcome: EventServerPublishOutcome;
  readonly status: number | undefined;
  readonly code: string | undefined;
  readonly retryAfterMs: number | undefined;
  readonly attempts: number;

  constructor(outcome: EventServerPublishOutcome, message: string, details: PublishErrorDetails = {}) {
    super(message);
    this.name = 'EventServerPublishError';
    this.outcome = outcome;
    this.status = details.status;
    this.code = details.code;
    this.retryAfterMs = details.retryAfterMs;
    this.attempts = details.attempts ?? 0;
  }
}

/** The request is known not to have been accepted and must not be retried automatically. */
export class EventServerPermanentPublishError extends EventServerPublishError {
  constructor(message: string, details: PublishErrorDetails = {}) {
    super('permanent', message, details);
    this.name = 'EventServerPermanentPublishError';
  }
}

/** The Event Server explicitly reported that the unchanged request may be retried. */
export class EventServerRetryablePublishError extends EventServerPublishError {
  constructor(message: string, details: PublishErrorDetails = {}) {
    super('retryable', message, details);
    this.name = 'EventServerRetryablePublishError';
  }
}

/** Acceptance is unknown; only the same Event Server route and stable identity may be retried. */
export class EventServerAmbiguousPublishError extends EventServerPublishError {
  constructor(message: string, details: PublishErrorDetails = {}) {
    super('ambiguous', message, details);
    this.name = 'EventServerAmbiguousPublishError';
  }
}

interface ResolvedConfig {
  endpoint: string;
  audience: string;
  tokenSource: EventServerTokenSource;
  requestTimeoutMs: number;
  maxRequestBytes: number;
  maxAttempts: number;
  baseDelayMs: number;
  maxDelayMs: number;
  fetch: typeof globalThis.fetch;
  random: () => number;
  sleep: (delayMs: number, signal: AbortSignal) => Promise<void>;
}

interface PublishContext {
  headers?: Headers | Record<string, string | undefined>;
  signal?: AbortSignal;
  traceparent?: string;
  tracestate?: string;
}

/**
 * Canonical provider-neutral HTTP publisher for the managed Event Server profile.
 *
 * It is intentionally publish-only: managed subscribers use the existing push
 * receiver, while self-managed pull/stream transports remain separate.
 */
export class EventServerPublisherTransport implements Transport {
  private readonly config: ResolvedConfig;

  constructor(config: EventServerTransportConfig) {
    this.config = resolveConfig(config);
  }

  async publish(topic: string, envelope: Envelope, options?: PublishOptions): Promise<void> {
    const frame = managedPublishFrame(topic, envelope);
    const body = encodeFrame(frame, this.config.maxRequestBytes);
    const context = tryContext<PublishContext>();
    const signal = options?.signal ?? context?.signal;
    const deadline = resolveDeadline(options?.deadline, this.config.requestTimeoutMs);
    const traceHeaders = resolveTraceHeaders(options, context);
    const requestSignal = deadlineSignal(signal, deadline);

    try {
      if (requestSignal.signal.aborted) {
        throw new EventServerRetryablePublishError('Event Server publish cancelled before send');
      }
      const token = await acquireToken(this.config, requestSignal.signal);
      await this.sendWithRetry(body, token, traceHeaders, requestSignal.signal, deadline, 1);
    } finally {
      requestSignal.cleanup();
    }
  }

  async subscribe(
    _definition: HandlerDefinition,
    _callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    throw new Error(
      'Event Server publisher transport does not support pull or stream subscriptions; use push delivery.',
    );
  }

  async start(): Promise<void> {}

  async stop(): Promise<void> {}

  private async send(
    body: string,
    token: string,
    traceHeaders: Record<string, string>,
    signal: AbortSignal,
    attempt: number,
  ): Promise<EventServerPublishRef | EventServerPublishError> {
    let response: Response;
    try {
      response = await this.config.fetch(this.config.endpoint, {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${token}`,
          'Content-Type': 'application/json',
          ...traceHeaders,
        },
        body,
        signal,
      });
    } catch {
      return new EventServerAmbiguousPublishError('Event Server request failed after send', { attempts: attempt });
    }

    const parsed = await readStructuredBody(response);
    if (response.status === 200) {
      if (isMatchingPublishRef(parsed, JSON.parse(body) as ManagedEventPublishFrame)) {
        return parsed;
      }
      return new EventServerAmbiguousPublishError('Event Server returned a malformed or mismatched success response', {
        status: response.status,
        attempts: attempt,
      });
    }

    const structured = isEventServerError(parsed) ? parsed : undefined;
    const code = structured?.code;
    if (
      (response.status === 400 || response.status === 401 || response.status === 403) &&
      structured?.retryable === false
    ) {
      return new EventServerPermanentPublishError(
        `Event Server permanently rejected the publish (HTTP ${response.status})`,
        {
          status: response.status,
          code,
          attempts: attempt,
        },
      );
    }

    if ((response.status === 429 || response.status === 503) && structured?.retryable === true) {
      return new EventServerRetryablePublishError(
        `Event Server asked the publisher to retry (HTTP ${response.status})`,
        {
          status: response.status,
          code,
          retryAfterMs: parseRetryAfter(response.headers.get('retry-after')),
          attempts: attempt,
        },
      );
    }

    return new EventServerAmbiguousPublishError(
      `Event Server returned an unrecognized response (HTTP ${response.status})`,
      {
        status: response.status,
        code,
        attempts: attempt,
      },
    );
  }

  private async sendWithRetry(
    body: string,
    token: string,
    traceHeaders: Record<string, string>,
    signal: AbortSignal,
    deadline: number,
    attempt: number,
  ): Promise<void> {
    const outcome = await this.send(body, token, traceHeaders, signal, attempt);
    if (!(outcome instanceof EventServerPublishError)) return;
    if (outcome.outcome === 'permanent' || attempt >= this.config.maxAttempts) throw outcome;

    const delayMs = retryDelay(outcome, attempt, this.config);
    if (Date.now() + delayMs >= deadline) throw outcome;
    try {
      await this.config.sleep(delayMs, signal);
    } catch {
      throw outcome;
    }
    return this.sendWithRetry(body, token, traceHeaders, signal, deadline, attempt + 1);
  }
}

/** Create the canonical managed Event Server publisher transport. */
export function eventServerTransport(config: EventServerTransportConfig): EventServerPublisherTransport {
  return new EventServerPublisherTransport(config);
}

/** Build and validate the strict managed-workload publish frame. */
export function managedPublishFrame(topic: string, envelope: Envelope): ManagedEventPublishFrame {
  validateManagedEnvelope(topic, envelope);
  const dedupeKey = resolveDedupeKey(envelope);
  const attributes = managedAttributes(envelope.attributes);

  const frame = compact({
    protocol: PUTNAMI_EVENTS_PROTOCOL,
    type: EVENT_FRAME_TYPES.publish,
    id: envelope.id,
    dedupeKey,
    topic: envelope.topic,
    topicVersion: envelope.topicVersion as string,
    payload: envelope.payload,
    key: envelope.key,
    attributes: Object.keys(attributes).length > 0 ? attributes : undefined,
    traceId: envelope.traceId,
  });
  if (!isManagedPublishFrame(frame)) {
    throw new EventServerPermanentPublishError('Event Server publish violates the canonical managed profile');
  }
  return frame;
}

function validateManagedEnvelope(topic: string, envelope: Envelope): void {
  if (!topic || topic !== envelope.topic) {
    throw new EventServerPermanentPublishError('Event Server publish topic does not match the envelope');
  }
  if (!envelope.id?.trim()) {
    throw new EventServerPermanentPublishError('Event Server publish requires a stable event id');
  }
  if (!envelope.topic.trim()) {
    throw new EventServerPermanentPublishError('Event Server publish requires a non-empty logical topic');
  }
  if (envelope.channel) {
    throw new EventServerPermanentPublishError('Event Server publish channel is managed server authority');
  }
  if (!envelope.topicVersion?.trim()) {
    throw new EventServerPermanentPublishError('Event Server publish requires a non-empty topic version');
  }
  if (envelope.payload === undefined) {
    throw new EventServerPermanentPublishError('Event Server publish requires a JSON payload');
  }
}

function resolveDedupeKey(envelope: Envelope): string {
  const dedupeKey = envelope.dedupeKey ?? envelope.id;
  if (!dedupeKey.trim()) {
    throw new EventServerPermanentPublishError('Event Server publish requires a stable dedupe key');
  }
  return dedupeKey;
}

function managedAttributes(source: Record<string, string> | undefined): Record<string, string> {
  const attributes: Record<string, string> = {};
  for (const [name, value] of Object.entries(source ?? {})) {
    if (isManagedPublishReservedAttribute(name)) {
      throw new EventServerPermanentPublishError(`Event Server publish attribute '${name}' is reserved`);
    }
    if (typeof value !== 'string') {
      throw new EventServerPermanentPublishError(`Event Server publish attribute '${name}' must be a string`);
    }
    attributes[name] = value;
  }
  return attributes;
}

function resolveConfig(config: EventServerTransportConfig): ResolvedConfig {
  if (config.contractVersion !== MANAGED_PUBLISH_CONTRACT_VERSION) {
    throw new Error(`events.eventServer.contractVersion must be ${MANAGED_PUBLISH_CONTRACT_VERSION}`);
  }
  if (config.protocol !== PUTNAMI_EVENTS_PROTOCOL) {
    throw new Error(`events.eventServer.protocol must be '${PUTNAMI_EVENTS_PROTOCOL}'`);
  }
  if (!config.audience?.trim()) {
    throw new Error('events.eventServer.audience is required');
  }
  if (typeof config.tokenSource !== 'function') {
    throw new Error('events.eventServer.tokenSource is required');
  }

  const endpoint = canonicalPublishEndpoint(config.endpoint);
  const maxAttempts = boundedInteger(config.retry?.maxAttempts, DEFAULT_MAX_ATTEMPTS, 1, 10, 'maxAttempts');
  const baseDelayMs = boundedInteger(config.retry?.baseDelayMs, DEFAULT_BASE_DELAY_MS, 0, 60_000, 'baseDelayMs');
  const maxDelayMs = boundedInteger(config.retry?.maxDelayMs, DEFAULT_MAX_DELAY_MS, 0, 60_000, 'maxDelayMs');
  if (maxDelayMs < baseDelayMs) {
    throw new Error('events.eventServer.retry.maxDelayMs must be greater than or equal to baseDelayMs');
  }

  return {
    endpoint,
    audience: config.audience,
    tokenSource: config.tokenSource,
    requestTimeoutMs: boundedInteger(
      config.requestTimeoutMs,
      DEFAULT_REQUEST_TIMEOUT_MS,
      1,
      300_000,
      'requestTimeoutMs',
    ),
    maxRequestBytes: boundedInteger(
      config.maxRequestBytes,
      DEFAULT_MAX_REQUEST_BYTES,
      1,
      16_777_216,
      'maxRequestBytes',
    ),
    maxAttempts,
    baseDelayMs,
    maxDelayMs,
    fetch: config.fetch ?? globalThis.fetch,
    random: config.random ?? Math.random,
    sleep: config.sleep ?? abortableSleep,
  };
}

function canonicalPublishEndpoint(value: string): string {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new Error('events.eventServer.endpoint must be a valid HTTP(S) origin');
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') {
    throw new Error('events.eventServer.endpoint must use http:// or https://');
  }
  if (url.username || url.password || url.search || url.hash || (url.pathname && url.pathname !== '/')) {
    throw new Error('events.eventServer.endpoint must be an origin without credentials, path, query, or fragment');
  }
  url.pathname = EVENT_SERVER_DEFAULT_ENDPOINTS.publish;
  return url.toString();
}

function encodeFrame(frame: ManagedEventPublishFrame, maxBytes: number): string {
  let body: string;
  try {
    body = JSON.stringify(frame);
  } catch {
    throw new EventServerPermanentPublishError('Event Server publish frame is not valid JSON');
  }
  if (new TextEncoder().encode(body).byteLength > maxBytes) {
    throw new EventServerPermanentPublishError('Event Server publish frame exceeds the configured size limit', {
      code: EVENT_SERVER_ERROR_CODES.payloadTooLarge,
    });
  }
  return body;
}

function compact<T extends Record<string, unknown>>(value: T): T {
  return Object.fromEntries(Object.entries(value).filter(([, field]) => field !== undefined)) as T;
}

function resolveDeadline(deadline: PublishOptions['deadline'], timeoutMs: number): number {
  const configured = Date.now() + timeoutMs;
  if (deadline === undefined) return configured;
  const requested = deadline instanceof Date ? deadline.getTime() : deadline;
  if (!Number.isFinite(requested)) {
    throw new EventServerPermanentPublishError('Event Server publish deadline must be a finite timestamp');
  }
  return Math.min(configured, requested);
}

function resolveTraceHeaders(
  options: PublishOptions | undefined,
  context: PublishContext | undefined,
): Record<string, string> {
  const traceparent = options?.traceparent ?? headerValue(context, 'traceparent') ?? context?.['traceparent'];
  const tracestate = options?.tracestate ?? headerValue(context, 'tracestate') ?? context?.['tracestate'];
  const headers: Record<string, string> = {};
  if (safeHeaderValue(traceparent)) headers['traceparent'] = traceparent;
  if (safeHeaderValue(tracestate)) headers['tracestate'] = tracestate;
  return headers;
}

function headerValue(context: PublishContext | undefined, name: string): string | undefined {
  if (context?.headers instanceof Headers) {
    return context.headers.get(name) ?? undefined;
  }
  const headers = context?.headers;
  if (!headers) return undefined;
  return headers[name] ?? headers[name.toLowerCase()] ?? headers[name.toUpperCase()];
}

function safeHeaderValue(value: string | undefined): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= 512 && !/[\r\n]/.test(value);
}

function validBearerToken(value: string): boolean {
  return typeof value === 'string' && value.trim().length > 0 && value === value.trim() && !/[\r\n]/.test(value);
}

async function acquireToken(config: ResolvedConfig, signal: AbortSignal): Promise<string> {
  let token: string;
  try {
    token = await raceWithAbort(config.tokenSource({ audience: config.audience, signal }), signal);
  } catch {
    throw new EventServerRetryablePublishError('Event Server credential source failed before send');
  }
  if (!validBearerToken(token)) {
    throw new EventServerPermanentPublishError('Event Server credential source returned an invalid token');
  }
  return token;
}

function deadlineSignal(
  parent: AbortSignal | undefined,
  deadline: number,
): { signal: AbortSignal; cleanup: () => void } {
  const controller = new AbortController();
  const abort = () => controller.abort();
  if (parent?.aborted || deadline <= Date.now()) {
    controller.abort();
    return { signal: controller.signal, cleanup: () => undefined };
  }
  parent?.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(abort, Math.max(0, deadline - Date.now()));
  return {
    signal: controller.signal,
    cleanup: () => {
      clearTimeout(timer);
      parent?.removeEventListener('abort', abort);
    },
  };
}

async function readStructuredBody(response: Response): Promise<unknown> {
  const contentLength = Number(response.headers.get('content-length') ?? '0');
  if (Number.isFinite(contentLength) && contentLength > MAX_ERROR_RESPONSE_BYTES) return undefined;
  let body: string;
  try {
    body = await response.text();
  } catch {
    return undefined;
  }
  if (!body || new TextEncoder().encode(body).byteLength > MAX_ERROR_RESPONSE_BYTES) return undefined;
  try {
    return JSON.parse(body);
  } catch {
    return undefined;
  }
}

function isMatchingPublishRef(value: unknown, frame: ManagedEventPublishFrame): value is EventServerPublishRef {
  if (typeof value !== 'object' || value === null) return false;
  const ref = value as Partial<EventServerPublishRef>;
  return (
    ref.protocol === PUTNAMI_EVENTS_PROTOCOL &&
    ref.id === frame.id &&
    ref.topic === frame.topic &&
    typeof ref.timestamp === 'string' &&
    ref.timestamp.length > 0 &&
    !Number.isNaN(Date.parse(ref.timestamp))
  );
}

interface StructuredEventServerError {
  protocol: typeof PUTNAMI_EVENTS_PROTOCOL;
  code: string;
  message: string;
  retryable?: boolean;
}

function isEventServerError(value: unknown): value is StructuredEventServerError {
  if (typeof value !== 'object' || value === null) return false;
  const error = value as Partial<StructuredEventServerError>;
  return (
    error.protocol === PUTNAMI_EVENTS_PROTOCOL &&
    typeof error.code === 'string' &&
    error.code.length > 0 &&
    typeof error.message === 'string' &&
    error.message.length > 0 &&
    (error.retryable === undefined || typeof error.retryable === 'boolean')
  );
}

function parseRetryAfter(value: string | null): number | undefined {
  if (!value) return undefined;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0) return Math.round(seconds * 1000);
  const timestamp = Date.parse(value);
  if (Number.isNaN(timestamp)) return undefined;
  return Math.max(0, timestamp - Date.now());
}

function retryDelay(error: EventServerPublishError, attempt: number, config: ResolvedConfig): number {
  const ceiling = Math.min(config.maxDelayMs, config.baseDelayMs * 2 ** (attempt - 1));
  const jittered = Math.round(config.random() * ceiling);
  return Math.max(jittered, error.retryAfterMs ?? 0);
}

function raceWithAbort<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) return Promise.reject(new Error('aborted'));
  return new Promise((resolve, reject) => {
    const onAbort = () => reject(new Error('aborted'));
    signal.addEventListener('abort', onAbort, { once: true });
    promise.then(
      (value) => {
        signal.removeEventListener('abort', onAbort);
        resolve(value);
      },
      (error: unknown) => {
        signal.removeEventListener('abort', onAbort);
        reject(error);
      },
    );
  });
}

function abortableSleep(delayMs: number, signal: AbortSignal): Promise<void> {
  if (delayMs <= 0) return Promise.resolve();
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new Error('aborted'));
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(new Error('aborted'));
    };
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', onAbort);
      resolve();
    }, delayMs);
    signal.addEventListener('abort', onAbort, { once: true });
  });
}

function boundedInteger(value: number | undefined, fallback: number, min: number, max: number, name: string): number {
  const resolved = value ?? fallback;
  if (!Number.isInteger(resolved) || resolved < min || resolved > max) {
    throw new Error(`events.eventServer.${name} must be an integer between ${min} and ${max}`);
  }
  return resolved;
}
