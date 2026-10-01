import type { CircuitBreaker } from './circuit-breaker';
import { CircuitOpenError } from './circuit-breaker';
import { StreamRelay } from './connect-stream';
import {
  ClientCanceledError,
  ClientCredentialError,
  ClientDeadlineError,
  ClientError,
  ClientFrameworkError,
  ClientRequestEncodingError,
  ClientServiceConfigError,
} from './errors';
import type { StreamObserver } from './stream.type';

/**
 * One of the five phases a stream session traverses, in order and without
 * going back. A session always reaches `closed`, whatever path it takes.
 *
 * - `connecting` covers everything up to and including admission: credential
 *   resolution, the handshake, and the transport's acceptance check. Nothing
 *   has been delivered to the caller yet.
 * - `admitted` means the provider accepted the operation — a 2xx SSE response
 *   carrying the declared content type, the WebSocket `ready` frame, or the
 *   accepted Connect response headers. It is never the first application
 *   message.
 * - `active` means the first provider message reached the caller.
 * - `terminal` means exactly one of complete, error or cancel has been decided.
 * - `closed` means the budgets are released and the single call measurement is
 *   emitted.
 */
export type StreamPhase = 'connecting' | 'admitted' | 'active' | 'terminal' | 'closed';

/**
 * The four session bounds, plus the declared operation duration and the
 * heartbeat cadence.
 *
 * `sessionMs` is the declared operation duration (`resilience.timeoutMs`) and
 * leaves the session unbounded in time when it is `0`; the other budgets still
 * apply. `heartbeatMs` is `0` when the provider declared none — a heartbeat
 * nobody declared hides a dead socket instead of revealing it.
 */
export interface StreamBudgets {
  readonly handshakeMs: number;
  readonly idleMs: number;
  readonly sessionMs: number;
  readonly heartbeatMs: number;
  readonly maxFrameBytes: number;
  readonly maxBufferedMessages: number;
}

/** What one attempt reports back when it ends. */
export interface StreamAttemptResult {
  readonly status?: number;
  readonly error?: unknown;
}

/** One attempt: its own signal, bounded by the handshake budget, and its single finisher. */
export interface StreamAttempt {
  readonly signal: AbortSignal;
  finish(result: StreamAttemptResult): void;
}

/** What the single call measurement carries. */
export interface StreamCallResult {
  readonly attempts: number;
  readonly status?: number;
  readonly error?: unknown;
}

export interface StreamSessionOptions {
  readonly serviceId: string;
  readonly operationId: string;
  readonly protocol: 'sse' | 'websocket' | 'connect';
  readonly budgets: StreamBudgets;
  /**
   * The circuit this operation shares with its unary siblings. Every breaker
   * write in a stream lifecycle happens here and nowhere else.
   */
  readonly breaker?: CircuitBreaker;
  /** Statuses the declared circuit policy counts as provider failures. */
  readonly failureStatuses?: readonly number[];
  /** Starts the single call measurement. The finisher is called exactly once, at close. */
  readonly startCall?: () => (result: StreamCallResult) => void;
  /**
   * Resolves — and re-resolves, when an asynchronous acquisition has since
   * expired — the credentials for the next frame or request, applying them to
   * the outgoing request. It is called again at the send point, so it must be
   * safe to call more than once.
   */
  readonly credentials?: (signal: AbortSignal) => Promise<void>;
  /** Drops the service credentials the last resolution applied. */
  readonly invalidateCredentials?: () => void;
  /** Projects a terminal error through the generated error mapper. */
  readonly mapError?: (error: unknown) => Error;
  /** The caller's own cancel signal, which decides whether a pre-admission failure is theirs. */
  readonly callerSignal?: AbortSignal;
}

/**
 * One stream lifecycle: five phases, four budgets, a single breaker
 * observation, the credential re-check at the send point, a single terminal,
 * and a single call measurement.
 *
 * Every first-party stream transport — SSE, WebSocket, and Connect when it
 * lands — drives one of these instead of restating those rules.
 */
export class StreamSession<T> {
  private readonly options: StreamSessionOptions;
  private readonly relay: StreamRelay<T>;
  private readonly controller = new AbortController();
  private currentPhase: StreamPhase = 'connecting';
  private admitted = false;
  private dispatched = false;
  private invalidated = false;
  private attempts = 0;
  private lastStatus?: number;
  private terminalError?: Error;
  private handshakeDeadlineAt = 0;
  private sessionTimer?: ReturnType<typeof setTimeout>;
  private handshakeTimer?: ReturnType<typeof setTimeout>;
  private idleTimer?: ReturnType<typeof setTimeout>;
  private finishCall?: (result: StreamCallResult) => void;
  private closed = false;
  private callerAbort?: () => void;

  constructor(options: StreamSessionOptions) {
    this.options = options;
    this.relay = new StreamRelay<T>(options.budgets.maxBufferedMessages);
    this.relay.attach(() => this.cancel());
    this.finishCall = options.startCall?.();
    if (options.budgets.sessionMs > 0) {
      this.sessionTimer = setTimeout(() => this.fail(this.deadline()), options.budgets.sessionMs);
    }
    const caller = options.callerSignal;
    if (caller) {
      this.callerAbort = () => this.fail(callerTermination(caller, options));
      if (caller.aborted) queueMicrotask(this.callerAbort);
      else caller.addEventListener('abort', this.callerAbort, { once: true });
    }
  }

  /** The declared bounds this session applies. A transport reads them; it never restates them. */
  get budgets(): StreamBudgets {
    return this.options.budgets;
  }

  /**
   * Whether the provider ever admitted this session. A declared transport
   * fallback reads it: after admission there is nothing left to fall back to,
   * because the next wire would deliver what was already delivered.
   */
  get wasAdmitted(): boolean {
    return this.admitted;
  }

  /** The phase the session has reached. */
  get phase(): StreamPhase {
    return this.currentPhase;
  }

  /** Aborts when the session is cancelled, deadlined, or closed. */
  get signal(): AbortSignal {
    return this.controller.signal;
  }

  /** The observer handed to the caller. */
  observer(): StreamObserver<T> {
    return this.relay;
  }

  /** True once the caller cancelled through the observer. */
  get isCancelled(): boolean {
    return this.relay.isCancelled;
  }

  /**
   * True once the caller registered a message handler on the observer: a value
   * delivered from then on reaches them, rather than the retained queue.
   */
  get subscribed(): boolean {
    return this.relay.hasSubscriber;
  }

  /** Run `listener` whenever the caller registers a message handler; at once when one already is. */
  onSubscribe(listener: () => void): void {
    this.relay.onSubscribe(listener);
  }

  /**
   * Start one attempt. Its signal is bounded by the handshake budget, because
   * the handshake is the only work an attempt performs: after admission the
   * session, not the attempt, bounds the stream.
   */
  beginAttempt(): StreamAttempt {
    this.attempts += 1;
    const budget = this.options.budgets.handshakeMs;
    clearTimeout(this.handshakeTimer);
    this.handshakeDeadlineAt = budget > 0 ? Date.now() + budget : 0;
    // The handshake budget is a session timer, not a bound on the attempt's
    // signal: an admitted stream reads from the same signal for its whole life,
    // and a timeout left on it would cut a healthy stream at the handshake
    // budget.
    if (budget > 0) this.handshakeTimer = setTimeout(() => this.fail(this.deadline()), budget);
    let finished = false;
    return {
      signal: this.controller.signal,
      finish: (result) => {
        if (finished) return;
        finished = true;
        if (result.status !== undefined) this.lastStatus = result.status;
      },
    };
  }

  /** The absolute instant the handshake budget expires; `0` when none bounds the attempt. */
  handshakeDeadline(): number {
    return this.handshakeDeadlineAt;
  }

  /**
   * Resolve, and re-resolve an asynchronous acquisition that has since expired,
   * the credentials for the next frame or request.
   *
   * The resolution runs twice. The first pass is the acquisition; it may block
   * long enough for the value it returns to be past its expiry by the time the
   * session reaches the send point. The second pass is that send-point
   * re-check: the session keeps no expiry clock of its own — the credential
   * manager owns freshness — so it asks again, which returns the held value
   * while it is still fresh and acquires a new one when it is not.
   */
  async credentials(): Promise<void> {
    if (!this.options.credentials) return;
    await this.options.credentials(this.controller.signal);
    await this.options.credentials(this.controller.signal);
  }

  /** Drop the resolved service credential exactly once. */
  invalidateCredentials(): void {
    if (this.invalidated) return;
    this.invalidated = true;
    this.options.invalidateCredentials?.();
  }

  /**
   * Record that an attempt has left for the provider. Before it, a
   * pre-admission failure is local — configuration, credential acquisition, or
   * the caller — and writes nothing to the breaker, because nothing about the
   * provider's availability has been observed.
   */
  dispatch(): void {
    this.dispatched = true;
  }

  /** Connecting to admitted, recording the single breaker success. Idempotent. */
  admit(): void {
    if (this.currentPhase !== 'connecting') return;
    this.currentPhase = 'admitted';
    this.admitted = true;
    clearTimeout(this.handshakeTimer);
    this.handshakeTimer = undefined;
    this.options.breaker?.onSuccess();
    this.touchIdle();
  }

  /**
   * The current attempt's handshake completed: release its bound. The first
   * connection's handshake ends at {@link admit}; a continuation of an admitted
   * session — a declared SSE continuation, a WebSocket resume — ends its own
   * here, without admitting the session again. It resets no other budget: the
   * idle budget and the declared duration run across every connection
   * (clientcontract ADR 0013), and only what a connection reads touches the
   * idle budget.
   */
  endHandshake(): void {
    clearTimeout(this.handshakeTimer);
    this.handshakeTimer = undefined;
    this.handshakeDeadlineAt = 0;
  }

  /** Admitted to active, on the first delivered message. */
  activate(): void {
    if (this.currentPhase === 'admitted') this.currentPhase = 'active';
  }

  /** Reset the idle budget. It only runs after admission. */
  touchIdle(): void {
    if (!this.admitted || this.options.budgets.idleMs <= 0) return;
    clearTimeout(this.idleTimer);
    this.idleTimer = setTimeout(() => this.fail(this.deadline()), this.options.budgets.idleMs);
  }

  /**
   * Deliver one value to the caller. Past `maxBufferedMessages` the relay ends
   * the stream with the declared queue error rather than dropping in silence.
   */
  deliver(value: T): void {
    this.activate();
    this.touchIdle();
    this.relay.deliver(value);
  }

  /**
   * Record the sanitized terminal error and apply the phase breaker rule.
   * Returns the error the caller must surface; a second terminal is ignored and
   * the first one is returned.
   */
  fail(error: unknown): Error {
    const mapped = this.options.mapError?.(error) ?? this.normalize(error);
    if (this.currentPhase === 'terminal' || this.currentPhase === 'closed') return this.terminalError ?? mapped;
    const preAdmission = this.currentPhase === 'connecting';
    this.currentPhase = 'terminal';
    this.terminalError = mapped;
    this.releaseBudgets();
    // After admission the session writes nothing to the breaker: the provider
    // accepted, so a mid-stream break is a session fact, not an availability
    // fact.
    if (preAdmission) this.recordPreAdmissionFailure(mapped);
    this.relay.fail(mapped);
    this.controller.abort(mapped);
    return mapped;
  }

  /** Record the successful terminal. */
  complete(): void {
    if (this.currentPhase === 'terminal' || this.currentPhase === 'closed') return;
    const preAdmission = this.currentPhase === 'connecting';
    this.currentPhase = 'terminal';
    this.releaseBudgets();
    if (preAdmission) this.options.breaker?.onSuccess();
    this.relay.complete();
  }

  /**
   * Terminal to closed: release the budgets, cancel the session, and emit the
   * single call measurement. Idempotent.
   *
   * Closing a session that has no terminal yet — a caller close, or the client
   * being disposed — records the cancel terminal first, so the breaker probe is
   * released and the measurement carries the terminal code.
   */
  close(): void {
    if (this.closed) return;
    if (this.currentPhase !== 'terminal') {
      this.fail(new ClientCanceledError(this.options.serviceId, this.options.operationId));
    }
    this.closed = true;
    this.currentPhase = 'closed';
    this.releaseBudgets();
    this.controller.abort();
    if (this.callerAbort) this.options.callerSignal?.removeEventListener('abort', this.callerAbort);
    const finish = this.finishCall;
    this.finishCall = undefined;
    finish?.({
      attempts: this.attempts,
      ...(this.admitted && this.lastStatus !== undefined ? { status: this.lastStatus } : {}),
      ...(this.terminalError ? { error: this.terminalError } : {}),
    });
  }

  /** The sanitized terminal error, if any. */
  get error(): Error | undefined {
    return this.terminalError;
  }

  /** End the session from the caller's side: one terminal, then close. */
  private cancel(): void {
    if (this.currentPhase !== 'terminal' && this.currentPhase !== 'closed') {
      this.fail(new ClientCanceledError(this.options.serviceId, this.options.operationId));
    }
    this.close();
  }

  /**
   * The pre-admission breaker rule: a rejected request records nothing at all,
   * a failure that never reached the provider or that the caller caused
   * releases the probe without a verdict, a provider answer the declared
   * circuit policy does not count as a failure records a success, and
   * everything else records a failure.
   */
  private recordPreAdmissionFailure(error: Error): void {
    const breaker = this.options.breaker;
    if (!breaker) return;
    if (error instanceof CircuitOpenError) return;
    if (!this.dispatched || this.callerDone() || isLocalFailure(error)) {
      breaker.onIgnored();
      return;
    }
    const failureStatuses = this.options.failureStatuses ?? DEFAULT_FAILURE_STATUSES;
    if (error instanceof ClientError && error.status > 0 && !failureStatuses.includes(error.status)) {
      breaker.onSuccess();
      return;
    }
    breaker.onFailure();
  }

  private callerDone(): boolean {
    return this.options.callerSignal?.aborted ?? false;
  }

  /** Keep a typed error as-is; anything else becomes the opaque remote failure, named. */
  private normalize(error: unknown): Error {
    if (error instanceof ClientError) return error;
    if (this.callerDone()) return callerTermination(this.options.callerSignal as AbortSignal, this.options);
    if (error instanceof Error && (error.name === 'TimeoutError' || error.name === 'AbortError')) {
      return this.deadline();
    }
    return new ClientFrameworkError({
      service: this.options.serviceId,
      method: this.options.operationId,
      status: 0,
      code: 'client.remote',
    });
  }

  private deadline(): ClientDeadlineError {
    return new ClientDeadlineError(this.options.serviceId, this.options.operationId);
  }

  private releaseBudgets(): void {
    clearTimeout(this.sessionTimer);
    clearTimeout(this.idleTimer);
    clearTimeout(this.handshakeTimer);
    this.sessionTimer = undefined;
    this.idleTimer = undefined;
    this.handshakeTimer = undefined;
  }
}

/** Statuses the framework counts as provider failures when the contract declares none. */
const DEFAULT_FAILURE_STATUSES: readonly number[] = [500, 502, 503, 504];

/** A failure that never observed the provider: local configuration, encoding, or the caller. */
function isLocalFailure(error: unknown): boolean {
  return (
    error instanceof ClientCanceledError ||
    error instanceof ClientCredentialError ||
    error instanceof ClientRequestEncodingError ||
    error instanceof ClientServiceConfigError
  );
}

/** Whether the caller's abort was their own cancel or their deadline elapsing. */
function callerTermination(signal: AbortSignal, options: StreamSessionOptions): Error {
  const deadline = signal.reason instanceof Error && signal.reason.name === 'TimeoutError';
  return deadline
    ? new ClientDeadlineError(options.serviceId, options.operationId)
    : new ClientCanceledError(options.serviceId, options.operationId);
}
