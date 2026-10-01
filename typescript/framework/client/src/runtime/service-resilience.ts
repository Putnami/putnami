import type { ClientResiliencePolicy } from '@putnami/application';
import { tryContext } from '@putnami/runtime';
import { CircuitBreaker, CircuitOpenError, type CircuitBreakerConfig } from './circuit-breaker';
import {
  ClientCanceledError,
  ClientCredentialError,
  ClientDeadlineError,
  ClientError,
  ClientRequestEncodingError,
  ClientServiceConfigError,
} from './errors';
import type { ClientRequest, DisposableInterceptor, Interceptor } from './transport.type';

export const FIRST_PARTY_UNARY_DEFAULTS = Object.freeze({
  timeoutMs: 30_000,
  attemptTimeoutMs: 30_000,
  maxResponseBytes: 32 * 1024 * 1024,
  retryStatuses: Object.freeze([408, 429, 502, 503, 504]),
  retryBaseDelayMs: 200,
  retryMaxDelayMs: 5000,
  circuitFailureThreshold: 5,
  circuitResetTimeoutMs: 30_000,
  streamIdleTimeoutMs: 30_000,
  streamHeartbeatMs: 15_000,
  streamMaxBufferedMessages: 64,
  streamMaxFrameBytes: 1024 * 1024,
});

export interface ResolvedClientResilience {
  timeoutMs: number;
  attemptTimeoutMs: number;
  maxResponseBytes: number;
  retry: { maxAttempts: number; statuses: readonly number[]; codes: readonly string[] };
  circuit: { failureThreshold: number; resetTimeoutMs: number };
  stream: {
    handshakeTimeoutMs: number;
    idleTimeoutMs: number;
    heartbeatMs: number;
    reconnect: boolean;
    maxBufferedMessages: number;
    maxFrameBytes: number;
  };
}

/** Merge document then operation policy field-by-field over shared defaults. */
export function resolveClientResilience(request: ClientRequest): ResolvedClientResilience {
  const document = request.clientDefaults;
  const operation = request.clientOperation?.resilience;
  const merged = mergePolicy(document, operation);
  const kind = request.clientOperation?.idempotency.kind ?? 'non-idempotent';
  return {
    timeoutMs: positiveInteger(merged.timeoutMs, FIRST_PARTY_UNARY_DEFAULTS.timeoutMs),
    attemptTimeoutMs: positiveInteger(merged.attemptTimeoutMs, FIRST_PARTY_UNARY_DEFAULTS.attemptTimeoutMs),
    maxResponseBytes: positiveInteger(merged.maxResponseBytes, FIRST_PARTY_UNARY_DEFAULTS.maxResponseBytes),
    retry: {
      maxAttempts: kind === 'non-idempotent' ? 1 : positiveInteger(merged.retry?.maxAttempts, 4),
      statuses: merged.retry?.statuses ?? FIRST_PARTY_UNARY_DEFAULTS.retryStatuses,
      codes: merged.retry?.codes ?? [],
    },
    circuit: {
      failureThreshold: positiveInteger(
        merged.circuit?.failureThreshold,
        FIRST_PARTY_UNARY_DEFAULTS.circuitFailureThreshold,
      ),
      resetTimeoutMs: positiveInteger(merged.circuit?.resetTimeoutMs, FIRST_PARTY_UNARY_DEFAULTS.circuitResetTimeoutMs),
    },
    stream: {
      // Absent, the handshake budget falls back to the per-attempt timeout —
      // what both runtimes already did implicitly, now declarable.
      handshakeTimeoutMs: positiveInteger(
        merged.stream?.handshakeTimeoutMs,
        positiveInteger(merged.attemptTimeoutMs, FIRST_PARTY_UNARY_DEFAULTS.attemptTimeoutMs),
      ),
      idleTimeoutMs: positiveInteger(merged.stream?.idleTimeoutMs, FIRST_PARTY_UNARY_DEFAULTS.streamIdleTimeoutMs),
      heartbeatMs: positiveInteger(merged.stream?.heartbeatMs, FIRST_PARTY_UNARY_DEFAULTS.streamHeartbeatMs),
      reconnect: merged.stream?.reconnect ?? false,
      maxBufferedMessages: positiveInteger(
        merged.stream?.maxBufferedMessages,
        FIRST_PARTY_UNARY_DEFAULTS.streamMaxBufferedMessages,
      ),
      maxFrameBytes: positiveInteger(merged.stream?.maxFrameBytes, FIRST_PARTY_UNARY_DEFAULTS.streamMaxFrameBytes),
    },
  };
}

/** Total unary deadline applied outside auth, circuit, retry, and transport. */
export function serviceDeadlineInterceptor(): Interceptor {
  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one outer deadline combines provider, caller, and ambient budgets and normalizes both abort sources
  return async (request, next) => {
    if (request.clientOperation?.stream !== 'unary') return next(request);
    const context = request.detachedFromCaller
      ? undefined
      : tryContext<{ signal?: AbortSignal; deadlineAt?: number }>();
    const declaredTimeout = resolveClientResilience(request).timeoutMs;
    const inheritedDeadline = Math.min(
      request.deadlineAt ?? Number.POSITIVE_INFINITY,
      context?.deadlineAt ?? Number.POSITIVE_INFINITY,
    );
    const timeoutMs = Math.max(1, Math.min(declaredTimeout, inheritedDeadline - Date.now()));
    const timeout = AbortSignal.timeout(timeoutMs);
    const inheritedSignals = [request.signal, context?.signal].filter(
      (signal): signal is AbortSignal => signal !== undefined,
    );
    const inheritedSignal =
      inheritedSignals.length > 1
        ? AbortSignal.any(inheritedSignals)
        : inheritedSignals.length === 1
          ? inheritedSignals[0]
          : undefined;
    const signal = inheritedSignal ? AbortSignal.any([inheritedSignal, timeout]) : timeout;
    const deadlineAt = Math.min(inheritedDeadline, Date.now() + timeoutMs);
    try {
      return await next({ ...request, signal, deadlineAt });
    } catch (error) {
      if (inheritedSignal?.aborted) throw new ClientCanceledError('', `${request.method} ${request.path}`);
      if (timeout.aborted) throw new ClientDeadlineError('', `${request.method} ${request.path}`);
      throw error;
    }
  };
}

/**
 * Per-operation circuit state. Credential/config failures occur before this
 * interceptor and never affect service health.
 */
export function serviceCircuitInterceptor(serviceId?: string): DisposableInterceptor {
  const circuits = new Map<string, { breaker: CircuitBreaker; failureStatuses: ReadonlySet<number> }>();
  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: circuit admission must classify responses, service failures, and ignored local failures without state corruption
  const interceptor: Interceptor = async (request, next) => {
    if (!request.clientOperation) return next(request);
    const resilience = resolveClientResilience(request);
    const config: Partial<CircuitBreakerConfig> = {
      failureThreshold: resilience.circuit.failureThreshold,
      resetTimeoutMs: resilience.circuit.resetTimeoutMs,
      failureStatuses: [500, 502, 503, 504],
      ...(serviceId ? { serviceName: serviceId } : {}),
    };
    const key = `${request.operationId ?? request.path}:${JSON.stringify(config)}`;
    let circuit = circuits.get(key);
    if (!circuit) {
      circuit = { breaker: new CircuitBreaker(config), failureStatuses: new Set(config.failureStatuses) };
      circuits.set(key, circuit);
    }
    if (!circuit.breaker.allowRequest())
      throw new CircuitOpenError(circuit.breaker.getState(), '', `${request.method} ${request.path}`);
    try {
      const response = await next(request);
      if (circuit.failureStatuses.has(response.status)) circuit.breaker.onFailure();
      else circuit.breaker.onSuccess();
      return response;
    } catch (error) {
      if (isIgnoredCircuitError(error)) circuit.breaker.onIgnored();
      else if (!(error instanceof ClientError) || error.status === 0 || circuit.failureStatuses.has(error.status)) {
        circuit.breaker.onFailure();
      } else circuit.breaker.onSuccess();
      throw error;
    }
  };
  return Object.assign(interceptor, {
    dispose: () => {
      for (const circuit of circuits.values()) circuit.breaker.dispose();
      circuits.clear();
    },
  });
}

function isIgnoredCircuitError(error: unknown): boolean {
  return (
    error instanceof ClientCanceledError ||
    error instanceof ClientCredentialError ||
    error instanceof ClientRequestEncodingError ||
    error instanceof ClientServiceConfigError
  );
}

function mergePolicy(
  document: ClientResiliencePolicy | undefined,
  operation: ClientResiliencePolicy | undefined,
): ClientResiliencePolicy {
  return {
    ...document,
    ...operation,
    retry: { ...document?.retry, ...operation?.retry },
    circuit: { ...document?.circuit, ...operation?.circuit },
    stream: { ...document?.stream, ...operation?.stream },
  };
}

function positiveInteger(value: number | undefined, fallback: number): number {
  return value !== undefined && Number.isSafeInteger(value) && value > 0 ? value : fallback;
}
