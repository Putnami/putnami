import type { ClientSchema, ClientSseContinuation } from '@putnami/application';
import { decodeJsonValue, parseJsonValue } from './json-codec';
import { ClientError, ClientFrameworkError, ClientResponseContractError, decodeFrameworkError } from './errors';
import { readBodyTextCapped } from './response-cap';
import { decodeSseTerminalError, SseContinuation } from './sse-continuation';
import type { StreamSession } from './stream-session';
import type { StreamObserver } from './stream.type';
import type { ClientRequest } from './transport.type';
import { assertHttpUrl, buildRequestUrl } from './url';

const EVENT_STREAM = 'text/event-stream';

export interface SseStreamOptions {
  output: ClientSchema;
  /**
   * The SSE transport's declared continuation (clientcontract ADR 0013). When
   * present the stream speaks the negotiated wire on every connection: the
   * request asks for it, a provider that does not acknowledge it is refused
   * before any message, and the stream ends only at an explicit terminal.
   * Absent, the stream keeps the legacy framing byte for byte.
   */
  continuation?: ClientSseContinuation;
  /**
   * The operation's effective `resilience.stream.reconnect` on a safe stream.
   * With a continuation declared, an interruption then reopens the same
   * operation in the same session; without it, the interruption is the
   * terminal. Read only beside `continuation`.
   */
  reconnect?: boolean;
}

/**
 * Strict first-party SSE transport with bounded frames, idle time, and schema
 * decoding.
 *
 * Every lifecycle fact — admission, the single terminal, the breaker verdict,
 * the credential invalidation, the four budgets and the single call
 * measurement — belongs to the {@link StreamSession} it is handed. This
 * transport only turns bytes into values.
 */
export class SseTransport {
  private readonly baseUrl: string;
  private readonly serviceId: string;

  constructor(baseUrl: string, serviceId: string) {
    const validated = assertHttpUrl(baseUrl, 'SseTransport baseUrl');
    this.baseUrl = validated.endsWith('/') ? validated.slice(0, -1) : validated;
    this.serviceId = serviceId;
  }

  stream<T>(request: ClientRequest, options: SseStreamOptions, session: StreamSession<T>): StreamObserver<T> {
    if (options.continuation) {
      new SseContinuation<T>(
        this.baseUrl,
        this.serviceId,
        request,
        { output: options.output, continuation: options.continuation, reconnect: options.reconnect === true },
        session,
      ).start();
      return session.observer();
    }
    const handlers: SseHandlers<T> = {
      message: (value) => session.deliver(value),
      error: (error) => {
        // A 401 or 403 on the terminal event invalidates the credential exactly
        // once — the session owns that, so it happens in one place for both the
        // rejected handshake and the mid-stream refusal.
        if (error instanceof ClientError && (error.status === 401 || error.status === 403)) {
          session.invalidateCredentials();
        }
        session.fail(error);
        session.close();
      },
      complete: () => {
        session.complete();
        session.close();
      },
    };
    void this.consume(request, options, session, handlers).catch((error) => handlers.error?.(error as Error));
    return session.observer();
  }

  private async consume<T>(
    request: ClientRequest,
    options: SseStreamOptions,
    session: StreamSession<T>,
    handlers: SseHandlers<T>,
  ): Promise<void> {
    const headers = new Headers(request.headers);
    headers.set('Accept', EVENT_STREAM);
    const attempt = session.beginAttempt();
    const maxFrameBytes = session.budgets.maxFrameBytes;
    session.dispatch();
    const response = await fetch(buildRequestUrl(this.baseUrl, request), {
      method: 'GET',
      headers,
      signal: attempt.signal,
      redirect: 'error',
    });
    attempt.finish({ status: response.status });
    if (!response.ok) {
      if (response.status === 401 || response.status === 403) session.invalidateCredentials();
      const text = await readBodyTextCapped(response, maxFrameBytes, {
        service: this.serviceId,
        method: request.operationId ?? request.path,
      });
      let payload: unknown;
      try {
        payload = text ? parseJsonValue(text) : undefined;
      } catch {
        throw new ClientResponseContractError('provider returned malformed stream error');
      }
      if (!request.clientOperation)
        throw new ClientFrameworkError({
          service: this.serviceId,
          method: request.path,
          status: response.status,
          code: 'client.remote',
        });
      throw decodeFrameworkError({
        service: this.serviceId,
        method: request.operationId ?? request.path,
        status: response.status,
        payload,
        operation: request.clientOperation,
        schemas: request.clientSchemas,
        secrets: request.secretValues,
        carryRemoteMessage: request.carryRemoteMessage,
      });
    }
    const mediaType = response.headers.get('content-type')?.split(';', 1)[0]?.trim().toLowerCase();
    if (mediaType !== EVENT_STREAM || !response.body) {
      throw new ClientResponseContractError('provider returned an invalid SSE response');
    }
    // Admission is the accepted response carrying the declared content type,
    // never the first application message: before it, nothing was delivered.
    session.admit();

    const reader = response.body.getReader();
    const decoder = new TextDecoder('utf-8', { fatal: true });
    let buffered = '';
    try {
      while (true) {
        // biome-ignore lint/performance/noAwaitInLoops: network chunks must be consumed sequentially
        const { done, value } = await reader.read();
        if (done) break;
        session.touchIdle();
        buffered += decoder.decode(value, { stream: true });
        const delivered = deliverEvents(buffered, request, this.serviceId, options.output, maxFrameBytes, handlers);
        buffered = delivered.buffered;
        if (delivered.terminal) return;
        if (new TextEncoder().encode(buffered).byteLength > maxFrameBytes)
          throw new ClientResponseContractError('provider SSE frame exceeds the declared maximum');
      }
      buffered += decoder.decode();
      if (buffered.trim().length > 0)
        throw new ClientResponseContractError('provider ended with an incomplete SSE frame');
      handlers.complete?.();
    } finally {
      await reader.cancel().catch(() => {});
    }
  }
}

interface SseHandlers<T> {
  message?: (data: T) => void;
  error?: (error: Error) => void;
  complete?: () => void;
}

function deliverEvents<T>(
  buffered: string,
  request: ClientRequest,
  serviceId: string,
  output: ClientSchema,
  maxFrameBytes: number,
  handlers: SseHandlers<T>,
): { buffered: string; terminal: boolean } {
  const normalized = buffered.replaceAll('\r\n', '\n');
  let boundary = normalized.indexOf('\n\n');
  let consumed = 0;
  while (boundary >= 0) {
    const event = normalized.slice(consumed, boundary);
    if (new TextEncoder().encode(event).byteLength > maxFrameBytes)
      throw new ClientResponseContractError('provider SSE frame exceeds the declared maximum');
    consumed = boundary + 2;
    const eventType = event
      .split('\n')
      .find((line) => line.startsWith('event:'))
      ?.slice(6)
      .trim();
    const data = event
      .split('\n')
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).replace(/^ /, ''))
      .join('\n');
    if (data) {
      const payload = parseJsonValue(data);
      if (eventType === 'error') {
        handlers.error?.(decodeSseTerminalError(payload, request, serviceId));
        return { buffered: '', terminal: true };
      }
      const decoded = decodeJsonValue(payload, output, request.clientSchemas);
      handlers.message?.(decoded as T);
    }
    boundary = normalized.indexOf('\n\n', consumed);
  }
  return { buffered: normalized.slice(consumed), terminal: false };
}
