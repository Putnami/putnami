import {
  incCounterWithAttributes,
  observeHistogramWithAttributes,
  startTelemetrySpan,
  type TelemetryAttributes,
} from '@putnami/application';
import { ClientError, ClientFrameworkError, ClientResponseContractError } from './errors';
import type { ClientRequest, ClientResponse, Interceptor } from './transport.type';

const CALLS = 'rpc.client.calls';
const ATTEMPTS = 'rpc.client.attempts';
const CALL_DURATION = 'rpc.client.duration';
const ATTEMPT_DURATION = 'rpc.client.attempt.duration';
const AUTH_DURATION = 'rpc.client.auth.duration';

/** Call span/metrics wrapped around deadline, backoff, auth, transport, and strict decoding. */
export function serviceCallTelemetryInterceptor(serviceId: string): Interceptor {
  return async (request, next) => {
    if (!request.clientOperation) return next(request);
    const started = performance.now();
    request.telemetryState = { attempts: 0 };
    const base = callAttributes(serviceId, request);
    const span = startTelemetrySpan({
      name: request.operationId ?? request.path,
      kind: 'client',
      attributes: base,
    });
    try {
      const response = await span.run(() => next(request));
      finishCall(span, base, started, { response });
      return response;
    } catch (error) {
      finishCall(span, base, started, { error, request });
      throw error;
    }
  };
}

/** Attempt span/metrics wrapped around credential acquisition and strict transport decoding. */
export function serviceAttemptTelemetryInterceptor(serviceId: string): Interceptor {
  return async (request, next) => {
    if (!request.clientOperation) return next(request);
    const started = performance.now();
    const base = {
      ...callAttributes(serviceId, request),
      'rpc.client.attempt': request.telemetryAttempt ?? 1,
    } satisfies TelemetryAttributes;
    const span = startTelemetrySpan({
      name: `${request.operationId ?? request.path} attempt`,
      kind: 'client',
      attributes: base,
    });
    span.inject(request.headers);
    try {
      const response = await span.run(() => next(request));
      if (response.data instanceof ReadableStream) {
        return {
          ...response,
          data: finishAttemptWithStream(
            response.data,
            (error) => {
              if (error !== undefined && !request.signal?.aborted && !(error instanceof ClientFrameworkError)) {
                error = new ClientResponseContractError('service response stream failed');
              }
              finishAttempt(span, base, started, request.authDurationMs ?? 0, {
                response,
                ...(error === undefined ? {} : { error, request }),
              });
            },
            request.signal,
          ),
        };
      }
      finishAttempt(span, base, started, request.authDurationMs ?? 0, { response });
      return response;
    } catch (error) {
      finishAttempt(span, base, started, request.authDurationMs ?? 0, { error, request });
      throw error;
    }
  };
}

function finishAttemptWithStream(
  source: ReadableStream<Uint8Array>,
  finish: (error?: unknown) => void,
  signal?: AbortSignal,
): ReadableStream<Uint8Array> {
  const reader = source.getReader();
  let finished = false;
  let sink: ReadableStreamDefaultController<Uint8Array>;
  const complete = (error?: unknown): void => {
    if (finished) return;
    finished = true;
    signal?.removeEventListener('abort', abort);
    finish(error);
  };
  const abort = (): void => {
    if (finished) return;
    const error = signal?.reason ?? new DOMException('The request was aborted', 'AbortError');
    complete(error);
    sink.error(error);
    void reader.cancel(error).catch(() => {});
  };
  const stream = new ReadableStream<Uint8Array>(
    {
      start(controller) {
        sink = controller;
      },
      async pull(controller) {
        try {
          const item = await reader.read();
          if (finished) return;
          if (item.done) {
            controller.close();
            complete();
          } else controller.enqueue(item.value);
        } catch (error) {
          if (finished) return;
          controller.error(error);
          complete(error ?? new ClientResponseContractError('service response stream failed'));
        }
      },
      async cancel(reason) {
        if (finished) return;
        // Claim completion before canceling: cancel can resolve a pending read
        // as EOF, which must not finish this attempt a second time.
        finished = true;
        signal?.removeEventListener('abort', abort);
        try {
          await reader.cancel(reason);
          finish(reason);
        } catch (error) {
          finish(error);
          throw error;
        }
      },
    },
    { highWaterMark: 0 },
  );
  signal?.addEventListener('abort', abort, { once: true });
  if (signal?.aborted) abort();
  return stream;
}

/**
 * Start the single call measurement of a stream session.
 *
 * A stream never traverses the interceptor chain — the transport owns the
 * socket for the whole conversation — so the call span and the call metrics
 * come from here instead of {@link serviceCallTelemetryInterceptor}. Both
 * paths emit the same names and attributes, so a stream and a unary call are
 * one series.
 *
 * The span is injected into the request before the transport reads it, so the
 * provider receives it as the parent — in the SSE or Connect request head, and
 * in the WebSocket init frame's propagation context. The protocol label is the
 * transport that carries this session, which is not the first declared one
 * after a fallback, and the status is the one the provider admitted it with.
 */
export function startServiceStreamCall(
  serviceId: string,
  request: ClientRequest,
  protocol: string,
): (outcome: { error?: unknown; status?: number }) => void {
  const started = performance.now();
  const base = { ...callAttributes(serviceId, request), 'network.protocol.name': protocol };
  const span = startTelemetrySpan({ name: request.operationId ?? request.path, kind: 'client', attributes: base });
  span.inject(request.headers);
  let finished = false;
  return (outcome) => {
    if (finished) return;
    finished = true;
    finishCall(span, base, started, {
      ...(outcome.status === undefined ? {} : { status: outcome.status }),
      ...(outcome.error === undefined ? {} : { error: outcome.error, request }),
    });
  };
}

type SpanHandle = ReturnType<typeof startTelemetrySpan>;
type Outcome = { response?: ClientResponse; status?: number; error?: unknown; request?: ClientRequest };

function finishCall(span: SpanHandle, base: TelemetryAttributes, started: number, outcome: Outcome): void {
  const result = resultAttributes(outcome);
  const attributes = { ...base, ...result.attributes };
  span.finish({ attributes: result.attributes, ...(result.code ? { code: result.code } : {}) });
  incCounterWithAttributes(CALLS, 1, attributes);
  observeHistogramWithAttributes(CALL_DURATION, performance.now() - started, attributes);
}

function finishAttempt(
  span: SpanHandle,
  base: TelemetryAttributes,
  started: number,
  authDurationMs: number,
  outcome: Outcome,
): void {
  const result = resultAttributes(outcome);
  const metricAttributes = { ...base, ...result.attributes };
  span.finish({
    attributes: { ...result.attributes, 'rpc.client.auth.duration_ms': authDurationMs },
    ...(result.code ? { code: result.code } : {}),
  });
  incCounterWithAttributes(ATTEMPTS, 1, metricAttributes);
  observeHistogramWithAttributes(ATTEMPT_DURATION, performance.now() - started, metricAttributes);
  observeHistogramWithAttributes(AUTH_DURATION, authDurationMs, metricAttributes);
}

function callAttributes(serviceId: string, request: ClientRequest): TelemetryAttributes {
  return {
    'rpc.system': 'putnami',
    'rpc.service': serviceId,
    'rpc.method': request.operationId ?? request.path,
    'network.protocol.name': request.clientOperation?.transports[0]?.protocol ?? 'rest-json',
  };
}

function resultAttributes(outcome: Outcome): { attributes: TelemetryAttributes; code?: string } {
  const status =
    outcome.response?.status ?? outcome.status ?? (outcome.error instanceof ClientError ? outcome.error.status : 0);
  const code = outcome.error === undefined ? undefined : stableErrorCode(outcome.error, outcome.request);
  return {
    attributes: {
      ...(status > 0 ? { 'http.response.status_code': status } : {}),
      ...(code ? { 'error.type': code } : {}),
    },
    ...(code ? { code } : {}),
  };
}

function stableErrorCode(error: unknown, request?: ClientRequest): string {
  if (error instanceof ClientFrameworkError) return error.code;
  if (error instanceof ClientError) {
    const code = (error as ClientError & { code?: unknown }).code;
    if (typeof code === 'string' && code.length > 0) return code;
  }
  if (request?.signal?.aborted) {
    return request.signal.reason instanceof Error && request.signal.reason.name === 'TimeoutError'
      ? 'client.deadline'
      : 'client.canceled';
  }
  if (error instanceof Error && error.name === 'TimeoutError') return 'client.deadline';
  return 'client.remote';
}
