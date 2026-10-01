import type { ClientSchema, ClientSseContinuation, ClientSseCursor } from '@putnami/application';
import {
  classifySseEvent,
  negotiatesSseWire,
  SSE_WIRE_HEADER,
  SSE_WIRE_V1,
  sseCursorValue,
  sseReopenQuery,
} from '@putnami/application';
import { applyForwardedUserToken, tryForwardedUserRefresh } from './credential';
import { ClientError, ClientFrameworkError, ClientResponseContractError, decodeFrameworkError } from './errors';
import { decodeJsonValue, parseJsonValue } from './json-codec';
import { readBodyTextCapped } from './response-cap';
import { MAX_STREAM_RESUME_ATTEMPTS } from './service-ws-transport';
import type { StreamSession } from './stream-session';
import type { ClientRequest } from './transport.type';
import { buildRequestUrl } from './url';

const EVENT_STREAM = 'text/event-stream';

/** What the generated operation declares about a continuable SSE stream. */
export interface SseContinuationOptions {
  readonly output: ClientSchema;
  /** The SSE transport's `sse.continuation` (clientcontract ADR 0013). */
  readonly continuation: ClientSseContinuation;
  /**
   * The operation's effective `resilience.stream.reconnect` on a safe stream:
   * whether an interruption reopens the same operation in the same session.
   */
  readonly reconnect: boolean;
}

/** One admitted connection of a negotiated SSE session. */
interface SseConnection {
  readonly response: Response;
}

/** One decoded message waiting for the caller, with the position it carries (empty in best-effort mode). */
interface SsePending<T> {
  readonly value: T;
  readonly position: string;
}

/** How one connection ended, as the reader reports it. */
type SseConnectionOutcome =
  /** The explicit successful terminal. */
  | { readonly kind: 'complete' }
  /** A typed error or a contract error: the session's terminal. */
  | { readonly kind: 'error'; readonly error: Error }
  /** The connection ended before a terminal while the session was live: the one outcome a continuation may follow. */
  | { readonly kind: 'interrupted'; readonly error: Error }
  /** The session ended on its own — the caller, a budget — while the connection was read. */
  | { readonly kind: 'ended' };

/**
 * The negotiated wire of clientcontract ADR 0013, run over one
 * {@link StreamSession}. Mirrors `openNegotiatedSSE` in the Go runtime.
 *
 * The wire ends only at an explicit terminal: `complete` ends the session
 * successfully and a typed error ends it with that error. An end of body or a
 * broken socket before a terminal is an interruption. When the operation
 * declares reconnect, an interruption reopens the same declared SSE operation,
 * in the same session, at most {@link MAX_STREAM_RESUME_ATTEMPTS} times. It
 * never falls back to another transport and never changes mode.
 *
 * Cursor mode reopens after the position of the last message the caller
 * received and drops the messages it had not received yet, which the provider
 * sends again. Best-effort mode reopens with the original query and keeps its
 * queue: nothing it drops could be asked for again.
 */
export class SseContinuation<T> {
  private readonly opener: SseOpener<T>;
  private readonly delivery: SseDelivery<T>;
  private readonly reader: NegotiatedSseReader<T>;

  constructor(
    baseUrl: string,
    serviceId: string,
    request: ClientRequest,
    private readonly options: SseContinuationOptions,
    private readonly session: StreamSession<T>,
  ) {
    this.opener = new SseOpener<T>(baseUrl, serviceId, request, session);
    this.delivery = new SseDelivery<T>(session, session.budgets.maxBufferedMessages);
    this.reader = new NegotiatedSseReader<T>(
      request,
      serviceId,
      options.output,
      session,
      this.delivery,
      options.continuation.mode === 'cursor' ? options.continuation.cursor : undefined,
    );
  }

  /** Open the first connection and run the session to its single terminal. */
  start(): void {
    void this.run().catch((error) => this.fail(error));
  }

  private async run(): Promise<void> {
    const query = this.opener.request.query;
    const first = await this.opener.open(query, false);
    // The provider answered 2xx with the declared content type and its
    // acknowledgment: that is the admission. It records the single breaker
    // success and starts the idle budget; no reopening admits the session
    // again.
    this.session.admit();
    let connection = first;
    let continuations = 0;
    for (;;) {
      // biome-ignore lint/performance/noAwaitInLoops: the session's connections are read in turn
      const outcome = await this.reader.read(connection.response);
      switch (outcome.kind) {
        case 'complete':
          this.delivery.finish();
          this.session.complete();
          this.session.close();
          return;
        case 'error':
          this.fail(outcome.error);
          return;
        case 'ended':
          this.session.close();
          return;
        case 'interrupted':
          break;
      }
      if (this.ended()) {
        this.session.close();
        return;
      }
      // The sixth break ends the session with the break as its error.
      if (!this.options.reconnect || continuations >= MAX_STREAM_RESUME_ATTEMPTS) {
        this.fail(outcome.error);
        return;
      }
      continuations += 1;
      const delivered = this.options.continuation.mode === 'cursor' ? this.delivery.reset() : '';
      try {
        // biome-ignore lint/performance/noAwaitInLoops: a reopening follows the interruption it continues
        connection = await this.opener.open(sseReopenQuery(this.options.continuation, query, delivered), true);
      } catch (error) {
        // The idle budget and the declared duration keep running while a
        // reopening dials; when one of them ended the session, it is the
        // reason, not the handshake it interrupted.
        if (this.ended()) {
          this.session.close();
          return;
        }
        this.fail(error);
        return;
      }
    }
  }

  private ended(): boolean {
    return this.session.phase === 'terminal' || this.session.phase === 'closed';
  }

  /** Record the single terminal. The values the caller has not taken yet reach them first. */
  private fail(error: unknown): void {
    this.delivery.finish();
    // A 401 or 403 on the terminal event invalidates the credential exactly
    // once — the session owns that, so it happens in one place for both the
    // rejected handshake and the mid-stream refusal.
    if (error instanceof ClientError && (error.status === 401 || error.status === 403)) {
      this.session.invalidateCredentials();
    }
    this.session.fail(error);
    this.session.close();
  }
}

/**
 * Opens the connections of one SSE session. The first opening and every
 * reopening run the same steps — an attempt, the handshake under its own
 * budget, the admission checks — so a continuation is never a cheaper path
 * into the provider than the stream it continues. A reopening also resolves
 * its credentials again first: the first connection carries the ones the
 * client resolved before it was opened.
 */
class SseOpener<T> {
  /**
   * A forwarded user credential the binding re-minted after a reopening was
   * refused with 401. It replaces the caller's for the rest of the session.
   */
  private forwardedToken = '';

  constructor(
    private readonly baseUrl: string,
    private readonly serviceId: string,
    readonly request: ClientRequest,
    private readonly session: StreamSession<T>,
  ) {}

  /**
   * Run one opening with `query`. A reopening the provider refuses with 401
   * re-mints a forwarded user credential once, through the binding's
   * `refresh`, and opens again: the bounded refresh a unary call already has,
   * and nothing more — no refresh on 403 and no second remint. The first
   * opening never re-mints; its 401 ends the session as it always has.
   */
  async open(query: ClientRequest['query'], reopening: boolean): Promise<SseConnection> {
    try {
      return await this.attempt(query, reopening);
    } catch (error) {
      const refresh = this.request.forwardedUserRefresh;
      if (!reopening || !refresh || !(error instanceof ClientError) || error.status !== 401 || this.ended()) {
        throw error;
      }
      const fresh = await tryForwardedUserRefresh(refresh, this.session.signal);
      if (!fresh) throw error;
      this.forwardedToken = fresh;
      return await this.attempt(query, reopening);
    }
  }

  private ended(): boolean {
    return this.session.phase === 'terminal' || this.session.phase === 'closed';
  }

  /**
   * Perform one opening: begin an attempt, resolve the credentials of a
   * reopening, send the request under the handshake budget and check what the
   * provider answered.
   */
  private async attempt(query: ClientRequest['query'], reopening: boolean): Promise<SseConnection> {
    const session = this.session;
    const request = this.request;
    const attempt = session.beginAttempt();
    if (reopening) {
      await session.credentials();
      if (this.forwardedToken) applyForwardedUserToken(request, this.forwardedToken);
    }
    const headers = new Headers(request.headers);
    headers.set('Accept', EVENT_STREAM);
    // A declared continuation asks for the negotiated wire on every opening.
    headers.set(SSE_WIRE_HEADER, SSE_WIRE_V1);
    session.dispatch();
    const response = await fetch(buildRequestUrl(this.baseUrl, { path: request.path, params: request.params, query }), {
      method: 'GET',
      headers,
      signal: attempt.signal,
      redirect: 'error',
    });
    attempt.finish({ status: response.status });
    if (!response.ok) {
      // The stream lifecycle contract invalidates the resolved credential
      // exactly once on a rejected identity at admission, and never replays
      // the operation. 403 counts with 401: both mean the provider refused
      // the credential this session carried.
      if (response.status === 401 || response.status === 403) session.invalidateCredentials();
      throw await this.refusal(response);
    }
    const mediaType = response.headers.get('content-type')?.split(';', 1)[0]?.trim().toLowerCase();
    if (mediaType !== EVENT_STREAM || !response.body) {
      await response.body?.cancel().catch(() => {});
      throw new ClientResponseContractError('provider returned an invalid SSE response', this.serviceId, this.method);
    }
    // A provider that did not acknowledge the negotiated wire predates the
    // declared continuation, or speaks another version of it. Its end of body
    // would mean nothing, so the exchange is refused before any message: no
    // legacy reading, no fallback, no retry — during a rolling deploy, a
    // reopening that lands on an older instance ends the same way.
    if (!negotiatesSseWire(response.headers.get(SSE_WIRE_HEADER))) {
      await response.body.cancel().catch(() => {});
      throw new ClientResponseContractError(
        `provider did not acknowledge ${SSE_WIRE_HEADER} ${SSE_WIRE_V1}; it predates the declared continuation`,
        this.serviceId,
        this.method,
      );
    }
    // A reopening's handshake ends here; the first connection's ends at the
    // admission the caller records.
    if (reopening) session.endHandshake();
    return { response };
  }

  /** The typed error a non-2xx answer carries, decoded like the legacy reader decodes it. */
  private async refusal(response: Response): Promise<Error> {
    const request = this.request;
    const text = await readBodyTextCapped(response, this.session.budgets.maxFrameBytes, {
      service: this.serviceId,
      method: this.method,
    });
    let payload: unknown;
    try {
      payload = text ? parseJsonValue(text) : undefined;
    } catch {
      return new ClientResponseContractError('provider returned malformed stream error', this.serviceId, this.method);
    }
    if (!request.clientOperation) {
      return new ClientFrameworkError({
        service: this.serviceId,
        method: request.path,
        status: response.status,
        code: 'client.remote',
      });
    }
    return decodeFrameworkError({
      service: this.serviceId,
      method: this.method,
      status: response.status,
      payload,
      operation: request.clientOperation,
      schemas: request.clientSchemas,
      secrets: request.secretValues,
      carryRemoteMessage: request.carryRemoteMessage,
    });
  }

  private get method(): string {
    return this.request.operationId ?? this.request.path;
  }
}

/** A read from the connection failed: the one error that is an interruption, never a terminal. */
class SseSocketBreak extends Error {
  constructor(cause: unknown) {
    super('service stream connection broke', { cause });
  }
}

/**
 * Reads connections of the negotiated wire. Mirrors `negotiatedSSEReader`.
 *
 * An event the break cut short is discarded: it was never delivered, so the
 * position does not include it. A complete event that is malformed, outside
 * the negotiated vocabulary, oversize, or without a valid position is a
 * contract error and never an interruption.
 */
class NegotiatedSseReader<T> {
  constructor(
    private readonly request: ClientRequest,
    private readonly serviceId: string,
    private readonly output: ClientSchema,
    private readonly session: StreamSession<T>,
    private readonly delivery: SseDelivery<T>,
    /** The declared position carrier in cursor mode, absent in best-effort mode. */
    private readonly cursor: ClientSseCursor | undefined,
  ) {}

  /** Read one connection to its end and report how it ended. */
  async read(response: Response): Promise<SseConnectionOutcome> {
    const body = response.body as ReadableStream<Uint8Array>;
    const reader = body.getReader();
    try {
      return await this.consume(reader);
    } catch (error) {
      if (this.ended()) return { kind: 'ended' };
      // The socket broke: an interruption on this wire, not a failure.
      if (error instanceof SseSocketBreak)
        return { kind: 'interrupted', error: interrupted(this.serviceId, this.method) };
      // Anything else is the session's terminal: a typed or contract error,
      // or what the caller's message handler threw, which ends the session
      // without a reopening, as the legacy reader ends it.
      return { kind: 'error', error: error instanceof Error ? error : new Error(String(error)) };
    } finally {
      await reader.cancel().catch(() => {});
    }
  }

  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one loop owns line framing, the frame bound and the terminal classification
  private async consume(reader: ReadableStreamDefaultReader<Uint8Array>): Promise<SseConnectionOutcome> {
    const maxFrameBytes = this.session.budgets.maxFrameBytes;
    const decoder = new TextDecoder('utf-8', { fatal: true });
    const encoder = new TextEncoder();
    let buffered = '';
    let eventType = '';
    let data: string[] = [];
    let blockBytes = 0;
    const oversize = (): SseConnectionOutcome => ({
      kind: 'error',
      error: new ClientResponseContractError(
        'provider SSE frame exceeds the declared maximum',
        this.serviceId,
        this.method,
      ),
    });
    for (;;) {
      // biome-ignore lint/performance/noAwaitInLoops: network chunks must be consumed sequentially
      const { done, value } = await reader.read().catch((error: unknown) => {
        throw new SseSocketBreak(error);
      });
      if (done) break;
      this.session.touchIdle();
      try {
        buffered += decoder.decode(value, { stream: true });
      } catch {
        return {
          kind: 'error',
          error: new ClientResponseContractError('provider SSE frame is not UTF-8', this.serviceId, this.method),
        };
      }
      let boundary = buffered.indexOf('\n');
      while (boundary >= 0) {
        const line = buffered.slice(0, boundary);
        buffered = buffered.slice(boundary + 1);
        blockBytes += encoder.encode(line).byteLength + 1;
        if (blockBytes > maxFrameBytes) return oversize();
        const trimmed = line.endsWith('\r') ? line.slice(0, -1) : line;
        if (trimmed === '') {
          // biome-ignore lint/performance/noAwaitInLoops: a queued value applies backpressure to the read
          const terminal = await this.dispatch(eventType, data);
          if (terminal) return terminal;
          eventType = '';
          data = [];
          blockBytes = 0;
        } else if (trimmed.startsWith(':')) {
          // A comment keeps the connection alive and never advances the position.
        } else if (trimmed.startsWith('event:')) {
          eventType = trimmed.slice(6).trim();
        } else if (trimmed.startsWith('data:')) {
          data.push(trimmed.slice(5).replace(/^ /, ''));
        }
        boundary = buffered.indexOf('\n');
      }
      if (blockBytes + encoder.encode(buffered).byteLength > maxFrameBytes) return oversize();
      if (this.ended()) return { kind: 'ended' };
    }
    if (this.ended()) return { kind: 'ended' };
    // The body ended before a terminal. A pending, incomplete event is
    // discarded: it was never delivered.
    return { kind: 'interrupted', error: interrupted(this.serviceId, this.method) };
  }

  /**
   * Act on one complete event block. Returns the outcome when the event ended
   * the connection, `undefined` otherwise.
   */
  private async dispatch(eventType: string, data: string[]): Promise<SseConnectionOutcome | undefined> {
    if (data.length === 0) {
      // A comment-only block keeps the connection alive. A typed block with
      // no data — `complete` without its payload among them — is outside the
      // closed vocabulary.
      if (eventType !== '') return this.contractError('service stream event carries no data');
      return undefined;
    }
    const payload = data.join('\n');
    let kind: ReturnType<typeof classifySseEvent>;
    try {
      kind = classifySseEvent(eventType, payload, true);
    } catch {
      // The classification names what the provider sent; the caller reads a
      // fixed message, like every other contract error of this reader.
      return this.contractError('service stream event is outside the negotiated wire vocabulary');
    }
    let parsed: unknown;
    try {
      parsed = parseJsonValue(payload);
    } catch {
      return this.contractError('service stream message cannot be decoded');
    }
    if (kind === 'complete') return { kind: 'complete' };
    if (kind === 'error') return { kind: 'error', error: decodeSseTerminalError(parsed, this.request, this.serviceId) };
    let message: T;
    try {
      message = decodeJsonValue(parsed, this.output, this.request.clientSchemas) as T;
    } catch (error) {
      if (error instanceof ClientError) return { kind: 'error', error };
      return this.contractError('service stream message does not match the generated contract');
    }
    let position = '';
    if (this.cursor) {
      // The position is copied verbatim: never parsed, compared or
      // incremented. A message that cannot say where it is ends the stream,
      // because a continuation after it could not be placed.
      try {
        position = sseCursorValue(parsed, this.cursor.outputField);
      } catch {
        return this.contractError('service stream message carries no valid position in its declared cursor field');
      }
    }
    await this.delivery.enqueue({ value: message, position });
    return undefined;
  }

  private contractError(message: string): SseConnectionOutcome {
    return { kind: 'error', error: new ClientResponseContractError(message, this.serviceId, this.method) };
  }

  private ended(): boolean {
    return this.session.phase === 'terminal' || this.session.phase === 'closed';
  }

  private get method(): string {
    return this.request.operationId ?? this.request.path;
  }
}

/** The terminal of a negotiated stream whose connection ended before a terminal event and that is not continued. */
function interrupted(serviceId: string, method: string): ClientResponseContractError {
  return new ClientResponseContractError('service stream was interrupted before its terminal event', serviceId, method);
}

/**
 * The delivery bridge of one negotiated SSE session. Mirrors `sseDelivery`.
 *
 * The reader decodes into a bounded internal queue. A value is handed to the
 * caller only while they have a message handler registered — the observer
 * boundary — so a completed handoff is an observable fact: the caller has
 * received the value. Only then is the value delivered, and only then does
 * its position become the one a continuation resumes after. A value retained
 * for a caller who has not subscribed yet is not delivered, and the position
 * never includes it.
 *
 * The queue holds at most `maxBufferedMessages` values, the bound the
 * observer's retained queue has; a full queue holds the reader instead of
 * dropping a value, and the session's end or its idle budget still release it.
 */
export class SseDelivery<T> {
  private readonly queue: SsePending<T>[] = [];
  private readonly capacity: number;
  private readonly waiters: (() => void)[] = [];
  /** The position of the last value the caller took. */
  private delivered = '';
  private draining = false;

  constructor(
    private readonly session: StreamSession<T>,
    capacity: number,
  ) {
    this.capacity = Math.max(1, capacity);
    session.onSubscribe(() => this.flush());
    session.signal.addEventListener('abort', () => this.release(), { once: true });
  }

  /** The position of the last value the caller received; empty before any. */
  get position(): string {
    return this.delivered;
  }

  /** How many decoded values the caller has not received yet. */
  get pending(): number {
    return this.queue.length;
  }

  /**
   * Queue one decoded value. The promise settles once the queue has room
   * for the next one, or the session ended: a full queue holds the reader
   * rather than dropping a value.
   */
  enqueue(pending: SsePending<T>): Promise<void> {
    this.queue.push(pending);
    this.flush();
    if (this.queue.length < this.capacity || this.session.signal.aborted) return Promise.resolve();
    return new Promise((resolve) => this.waiters.push(resolve));
  }

  /**
   * Settle, before a cursor-mode reopening, every value the caller has not
   * taken: drop them and return the position of the last value the caller did
   * take. The reopened connection asks for everything after that position, so
   * a dropped value is sent again and none arrives twice.
   */
  reset(): string {
    this.queue.length = 0;
    this.release();
    return this.delivered;
  }

  /**
   * Hand the caller what is still queued, in order, before the terminal. A
   * caller who has not subscribed yet finds them retained on the observer,
   * ahead of the terminal, as the legacy reader leaves them.
   */
  finish(): void {
    for (const head of this.queue.splice(0)) this.session.deliver(head.value);
    this.release();
  }

  /** Hand queued values to a subscribed caller, in order; each handoff advances the position. */
  private flush(): void {
    if (this.draining) return;
    this.draining = true;
    try {
      while (this.queue.length > 0 && this.session.subscribed && !this.session.signal.aborted) {
        const head = this.queue.shift() as SsePending<T>;
        this.session.deliver(head.value);
        if (head.position !== '') this.delivered = head.position;
      }
    } finally {
      this.draining = false;
    }
    if (this.queue.length < this.capacity) this.release();
  }

  private release(): void {
    for (const wake of this.waiters.splice(0)) wake();
  }
}

/** The typed terminal the `error` event carries, decoded like the legacy reader decodes it. */
export function decodeSseTerminalError(payload: unknown, request: ClientRequest, serviceId: string): Error {
  const status = isRecord(payload) ? decodeHttpStatus(payload['status']) : undefined;
  if (!isRecord(payload) || status === undefined || typeof payload['code'] !== 'string') {
    return new ClientFrameworkError({
      service: serviceId,
      method: request.operationId ?? request.path,
      status: 0,
      code: 'client.remote',
    });
  }
  if (!request.clientOperation) {
    return new ClientFrameworkError({
      service: serviceId,
      method: request.operationId ?? request.path,
      status,
      code: 'client.remote',
    });
  }
  return decodeFrameworkError({
    service: serviceId,
    method: request.operationId ?? request.path,
    status,
    payload,
    remoteCode: payload['code'],
    detailsPayload: payload['details'],
    operation: request.clientOperation,
    schemas: request.clientSchemas,
    secrets: request.secretValues,
    carryRemoteMessage: request.carryRemoteMessage,
  });
}

function decodeHttpStatus(value: unknown): number | undefined {
  try {
    const status = decodeJsonValue(value, { type: 'integer', format: 'int32', minimum: 100, maximum: 599 });
    return typeof status === 'number' ? status : undefined;
  } catch {
    return undefined;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
