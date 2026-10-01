import {
  type ClientSchema,
  parseWebSocketServiceFrameV1,
  SERVICE_WEBSOCKET_SUBPROTOCOL,
  WebSocketConversationV1,
  type WebSocketDiagnostic,
  type WebSocketInitFrame,
  type WebSocketServiceFrame,
  type WebSocketStreamMode,
} from '@putnami/application';
import {
  ClientCredentialError,
  ClientDeadlineError,
  ClientRequestEncodingError,
  ClientResponseContractError,
  ClientTransportUnavailableError,
  decodeFrameworkError,
} from './errors';
import { decodeJsonValue, encodeJsonBody } from './json-codec';
import type { StreamSession } from './stream-session';
import type { DuplexStream, StreamObserver } from './stream.type';
import type { ClientRequest } from './transport.type';
import { assertWebSocketUrl, buildRequestUrl } from './url';

/**
 * The framework bound on how many times one session continues over a new
 * socket. It is a bound, never a behavior: a stream continues only because the
 * provider declared resume and the operation declared reconnect, and this caps
 * how long a socket that keeps breaking may keep a session alive.
 */
export const MAX_STREAM_RESUME_ATTEMPTS = 5;

/** What the generated operation declares about the stream this socket carries. */
export interface ServiceWebSocketOptions {
  readonly stream: WebSocketStreamMode;
  readonly input?: ClientSchema;
  readonly output?: ClientSchema;
  /** Mirrors the selected transport's `websocket.resume` flag. */
  readonly resumeDeclared: boolean;
  /**
   * The operation's own `resilience.stream.reconnect`: the half of the resume
   * agreement that says this runtime may ask the provider to continue. Both
   * halves are provider declarations; neither is a consumer flag.
   */
  readonly reconnectDeclared?: boolean;
  /** Requests a resumed stream. Only legal on a transport that declares resume. */
  readonly resume?: { readonly token: string; readonly afterSequence: string };
}

/**
 * Strict first-party WebSocket v1 transport.
 *
 * The socket is opened the way a browser must open it — `new WebSocket(url,
 * 'putnami.service.v1')`, no headers, no query credential — and everything the
 * provider needs to admit the call travels in the `init` frame. The legacy raw
 * {@link WebSocketTransport} stays separate and untouched.
 */
export class ServiceWebSocketTransport {
  private readonly baseUrl: string;
  private readonly serviceId: string;

  constructor(baseUrl: string, serviceId: string) {
    this.baseUrl = assertWebSocketUrl(baseUrl, 'ServiceWebSocketTransport baseUrl')
      .replace(/^http/, 'ws')
      .replace(/\/$/, '');
    this.serviceId = serviceId;
  }

  /** Open a server stream: values arrive as messages, the result carries none. */
  stream<T>(request: ClientRequest, options: ServiceWebSocketOptions, session: StreamSession<T>): StreamObserver<T> {
    new ServiceWebSocketSession<never, T>(this.baseUrl, this.serviceId, request, options, session);
    return session.observer();
  }

  /** Open a client or bidirectional stream. Its single terminal value arrives as the last message. */
  streamDuplex<TIn, TOut>(
    request: ClientRequest,
    options: ServiceWebSocketOptions,
    session: StreamSession<TOut>,
  ): DuplexStream<TIn, TOut> {
    const conversation = new ServiceWebSocketSession<TIn, TOut>(
      this.baseUrl,
      this.serviceId,
      request,
      options,
      session,
    );
    const observer = session.observer();
    return {
      onMessage: (handler) => observer.onMessage(handler),
      onError: (handler) => observer.onError(handler),
      onComplete: (handler) => observer.onComplete(handler),
      cancel: () => observer.cancel(),
      send: (value) => conversation.send(value),
      end: () => conversation.end(),
    };
  }
}

/** True when this runtime exposes a `WebSocket` constructor. */
export function isWebSocketAvailable(): boolean {
  return typeof globalThis.WebSocket !== 'undefined';
}

class ServiceWebSocketSession<TIn, TOut> {
  private conversation: WebSocketConversationV1;
  private readonly pending: WebSocketServiceFrame[] = [];
  private socket?: WebSocket;
  private ready = false;
  private clientSequence = 0n;
  private halfClosed = false;
  private heartbeatTimer?: ReturnType<typeof setInterval>;
  private messageChain = Promise.resolve();
  /**
   * The sequence of the last provider message the caller completely received.
   * A message decoded but not handed over is not counted: a continuation that
   * skipped it would drop a value nobody read.
   */
  private delivered = 0n;
  /**
   * The last token the provider issued in its ready frame. It is provider
   * material: it never reaches a URL, a subprotocol or a log.
   */
  private resumeToken = '';
  /** How many continuations this session has already spent. */
  private resumeAttempts = 0;
  /** The resume request the socket being opened presents, if any. */
  private resuming?: { readonly token: string; readonly afterSequence: string };

  constructor(
    private readonly baseUrl: string,
    private readonly serviceId: string,
    private readonly request: ClientRequest,
    private readonly options: ServiceWebSocketOptions,
    private readonly session: StreamSession<TOut>,
  ) {
    this.conversation = this.newConversation();
    this.resuming = options.resume;
    // The session owns the lifecycle, so the socket follows it: whatever ends
    // the session — a caller cancel, a budget, a terminal frame — is reported
    // to the provider from one place, once, whichever socket is live.
    this.session.signal.addEventListener('abort', () => this.onSessionEnd(), { once: true });
    this.connect();
  }

  private newConversation(): WebSocketConversationV1 {
    return new WebSocketConversationV1({
      stream: this.options.stream,
      encoding: 'json',
      resumeDeclared: this.options.resumeDeclared,
    });
  }

  /**
   * Whether this session may continue over a new socket.
   *
   * Resume re-reads a position the caller already consumed up to, so it is only
   * sound where re-reading has no effect: a server stream on a transport whose
   * provider states it can continue without a gap, and only when the operation
   * declares reconnect.
   */
  private get mayResume(): boolean {
    return (
      this.options.stream === 'server' &&
      this.options.resumeDeclared &&
      this.options.reconnectDeclared === true &&
      this.resumeToken !== '' &&
      this.resumeAttempts < MAX_STREAM_RESUME_ATTEMPTS
    );
  }

  send(value: TIn): void {
    if (this.halfClosed || this.session.phase === 'terminal' || this.session.phase === 'closed') return;
    if (!this.options.input) {
      this.terminate(new ClientRequestEncodingError('generated websocket input schema is missing'));
      return;
    }
    let encoded: unknown;
    try {
      encoded = JSON.parse(encodeJsonBody(value, this.options.input, this.request.clientSchemas)) as unknown;
    } catch (error) {
      this.terminate(error);
      return;
    }
    const sequence = this.clientSequence + 1n;
    if (
      this.queueOrSend({
        v: 1,
        type: 'message',
        sequence: sequence.toString(),
        payload: { encoding: 'json', value: encoded },
      })
    ) {
      this.clientSequence = sequence;
    }
  }

  end(): void {
    if (this.halfClosed || this.session.phase === 'terminal' || this.session.phase === 'closed') return;
    this.halfClosed = true;
    this.queueOrSend({ v: 1, type: 'half-close' });
  }

  // -------------------------------------------------------------------------
  // Socket
  // -------------------------------------------------------------------------

  private connect(): void {
    if (!isWebSocketAvailable()) {
      this.terminate(
        new ClientTransportUnavailableError({
          service: this.serviceId,
          method: this.method,
          transport: 'websocket',
        }),
      );
      return;
    }
    if (!this.request.clientOperation) {
      this.terminate(new ClientResponseContractError('generated websocket operation metadata is missing'));
      return;
    }
    const attempt = this.session.beginAttempt();
    // The URL carries the path and the declared query only. A credential in a
    // URL survives in proxy logs and browser history, so it travels in `init`.
    const url = buildRequestUrl(this.baseUrl, this.request);
    let socket: WebSocket;
    try {
      socket = new WebSocket(url, SERVICE_WEBSOCKET_SUBPROTOCOL);
    } catch (error) {
      this.terminate(error);
      return;
    }
    this.socket = socket;
    this.session.dispatch();
    attempt.finish({});
    socket.addEventListener('open', () => this.onOpen());
    socket.addEventListener('message', (event) => {
      this.messageChain = this.messageChain
        .then(() => this.onMessage((event as MessageEvent).data))
        .catch((error) => this.terminate(error));
    });
    socket.addEventListener('error', () =>
      this.terminate(
        new ClientResponseContractError('service websocket transport failed', this.serviceId, this.method),
      ),
    );
    // The close runs behind the frames that arrived before it. Reading a frame
    // awaits its text, so a close handled at once would overtake a terminal
    // frame the provider sent in the same write — and report a socket that
    // closed without one.
    socket.addEventListener('close', (event) => {
      this.messageChain = this.messageChain
        .then(() => this.onClose(event as CloseEvent))
        .catch((error) => this.terminate(error));
    });
  }

  /**
   * The provider echoed the subprotocol, so the socket speaks the first-party
   * wire. The `init` frame is composed, read back by the strict parser and
   * accepted by the conversation before a byte leaves.
   */
  private onOpen(): void {
    if (this.session.phase === 'terminal' || this.session.phase === 'closed') return;
    // The WHATWG `protocol` member is the provider's selection. Bun reports the
    // requested token instead, so this guard is proved against a stub rather
    // than against a Bun server that declines to negotiate.
    if (this.socket && this.socket.protocol !== SERVICE_WEBSOCKET_SUBPROTOCOL) {
      // The provider serves WebSocket at this path, but not this wire. That is
      // the same class as a 404 on the SSE half: a declared transport fallback
      // may act on it, and nothing about the call has been decided.
      this.terminate(
        new ClientTransportUnavailableError({
          service: this.serviceId,
          method: this.method,
          transport: 'websocket',
          message: 'provider did not negotiate the first-party websocket subprotocol',
        }),
      );
      return;
    }
    let init: WebSocketInitFrame;
    try {
      init = this.buildInitFrame();
    } catch (error) {
      this.terminate(error);
      return;
    }
    this.sendFrame(init);
    this.startHeartbeat();
  }

  private async onMessage(input: unknown): Promise<void> {
    if (this.session.phase === 'terminal' || this.session.phase === 'closed') return;
    this.session.touchIdle();
    const raw = await messageText(input);
    const parsed = parseWebSocketServiceFrameV1(raw, this.maxFrameBytes);
    if (!parsed.frame) {
      this.terminate(contractError(parsed.diagnostics[0], this.serviceId, this.method));
      return;
    }
    const refusal = this.conversation.accept(parsed.frame, 'server-to-client');
    if (refusal.length > 0) {
      this.terminate(contractError(refusal[0], this.serviceId, this.method));
      return;
    }
    this.dispatch(parsed.frame);
  }

  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one frame switch owns admission, delivery, heartbeat and the single terminal
  private dispatch(frame: WebSocketServiceFrame): void {
    switch (frame.type) {
      case 'ready': {
        // Admission. A resumed stream is honored only when this client asked
        // for one; the conversation already refused the other case.
        //
        // The other direction is this runtime's own rule: a provider that
        // answers a continuation with a fresh stream would deliver the
        // sequences the caller already consumed a second time.
        if (this.resuming && frame.resumed !== true) {
          this.terminate(
            new ClientResponseContractError(
              'provider refused to continue the stream and offered a fresh one',
              this.serviceId,
              this.method,
            ),
          );
          return;
        }
        this.resuming = undefined;
        // Each ready frame rotates the token: the one this socket presented is
        // spent, and only the new one can continue the stream after it.
        this.resumeToken = frame.resumeToken ?? '';
        this.ready = true;
        this.session.admit();
        // A continuation's own handshake ends here too: the session was
        // admitted once, by the first socket, and this one's bound must not
        // outlive the ready frame it just read.
        this.session.endHandshake();
        for (const queued of this.pending.splice(0)) this.write(queued);
        return;
      }
      case 'ping':
        this.sendFrame({ v: 1, type: 'pong', nonce: frame.nonce });
        return;
      case 'pong':
        return;
      case 'message':
        if (this.deliver(frame.payload, 'provider websocket message violates the generated contract')) {
          this.delivered = BigInt(frame.sequence);
        }
        return;
      case 'result':
        // A client or bidirectional stream carries its single terminal value
        // here; a server stream never does, and the wire refuses it.
        if (frame.payload) this.deliver(frame.payload, 'provider websocket result violates the generated contract');
        this.session.complete();
        this.close();
        return;
      case 'error': {
        if (frame.error.status === 401 || frame.error.status === 403) {
          this.session.invalidateCredentials();
        }
        this.terminate(
          decodeFrameworkError({
            service: this.serviceId,
            method: this.method,
            status: frame.error.status,
            payload: frame.error,
            remoteCode: frame.error.code,
            detailsPayload: frame.error.details,
            operation: this.request.clientOperation as NonNullable<ClientRequest['clientOperation']>,
            schemas: this.request.clientSchemas,
            secrets: this.request.secretValues,
            carryRemoteMessage: this.request.carryRemoteMessage,
          }),
        );
        return;
      }
      default:
        this.terminate(
          new ClientResponseContractError(
            'provider websocket frame is invalid in the current state',
            this.serviceId,
            this.method,
          ),
        );
    }
  }

  /** Hand one payload to the caller. Reports whether it reached them. */
  private deliver(payload: { encoding: string; value?: unknown }, message: string): boolean {
    if (payload.encoding !== 'json' || !this.options.output) {
      this.terminate(new ClientResponseContractError(message, this.serviceId, this.method));
      return false;
    }
    this.session.deliver(decodeJsonValue(payload.value, this.options.output, this.request.clientSchemas) as TOut);
    return true;
  }

  private onClose(event: CloseEvent): void {
    this.release();
    if (this.session.phase === 'terminal' || this.session.phase === 'closed') {
      this.session.close();
      return;
    }
    // The socket ended without a terminal frame. When the declaration allows
    // continuing, that is a transport break and not the end of the stream: a
    // new socket presents the provider's own token and the last sequence the
    // caller received, so nothing is skipped and nothing arrives twice.
    if (this.mayResume) {
      this.resume();
      return;
    }
    // Report it as a contract failure rather than as a silent completion; the
    // close reason is never quoted, because an intermediary chose it.
    this.terminate(
      new ClientResponseContractError(
        `provider closed the websocket without a terminal frame (${event.code})`,
        this.serviceId,
        this.method,
      ),
    );
  }

  /**
   * Continue this session over a new socket.
   *
   * Credentials are re-resolved first, so the continuation carries one that is
   * valid at the instant it dials rather than the one the broken socket
   * carried. A re-resolution that fails ends the session before admission,
   * which is exactly where the lifecycle contract allows a stream to stop.
   */
  private resume(): void {
    this.resumeAttempts += 1;
    this.resuming = { token: this.resumeToken, afterSequence: this.delivered.toString() };
    this.socket = undefined;
    this.ready = false;
    this.pending.length = 0;
    this.conversation = this.newConversation();
    void this.session
      .credentials()
      .then(() => {
        if (this.session.phase === 'terminal' || this.session.phase === 'closed') return;
        this.connect();
      })
      .catch((error) => this.terminate(error));
  }

  // -------------------------------------------------------------------------
  // Frames out
  // -------------------------------------------------------------------------

  private queueOrSend(frame: WebSocketServiceFrame): boolean {
    if (this.ready) return this.sendFrame(frame);
    if (this.pending.length >= this.session.budgets.maxBufferedMessages) {
      this.terminate(new ClientRequestEncodingError('service stream send queue exceeds the declared maximum'));
      return false;
    }
    // The frame is validated now, while the caller is still on the stack, so a
    // contract violation names the call that produced it.
    const data = JSON.stringify(frame);
    const parsed = parseWebSocketServiceFrameV1(data, this.maxFrameBytes);
    if (!parsed.frame) {
      this.terminate(contractError(parsed.diagnostics[0], this.serviceId, this.method));
      return false;
    }
    this.pending.push(frame);
    return true;
  }

  /**
   * Write one frame after the wire has read it back and the conversation has
   * accepted it. A frame this client composed that the wire refuses is a local
   * fault carrying the contract's own diagnostic code, never a code the runtime
   * invented.
   */
  private sendFrame(frame: WebSocketServiceFrame): boolean {
    const data = JSON.stringify(frame);
    const parsed = parseWebSocketServiceFrameV1(data, this.maxFrameBytes);
    if (!parsed.frame) {
      this.terminate(contractError(parsed.diagnostics[0], this.serviceId, this.method));
      return false;
    }
    const refusal = this.conversation.accept(parsed.frame, 'client-to-server');
    if (refusal.length > 0) {
      this.terminate(contractError(refusal[0], this.serviceId, this.method));
      return false;
    }
    this.write(data);
    return true;
  }

  private write(frame: WebSocketServiceFrame | string): void {
    try {
      this.socket?.send(typeof frame === 'string' ? frame : JSON.stringify(frame));
    } catch {
      // The socket is already gone; the close handler owns the terminal.
    }
  }

  private buildInitFrame(): WebSocketInitFrame {
    const clientId = this.request.headers.get('X-Client-Id');
    if (!clientId) throw new ClientCredentialError('generated websocket request has no client identity');
    const excluded = new Set([
      'x-client-id',
      'authorization',
      'traceparent',
      'tracestate',
      'baggage',
      'x-request-id',
      ...(this.request.credentialHeaderNames ?? []).map((name) => name.toLowerCase()),
    ]);
    const headers = [...this.request.headers.entries()]
      .filter(([name]) => !excluded.has(name.toLowerCase()))
      .map(([name, value]) => ({ name, values: [value] }));
    const deadlineAt = this.request.deadlineAt;
    const budget = deadlineAt === undefined ? 0 : Math.max(0, deadlineAt - Date.now());
    return {
      v: 1,
      type: 'init',
      operationId: this.request.operationId ?? this.request.path,
      clientId,
      deadlineUnixMs: deadlineAt === undefined ? '0' : Math.trunc(deadlineAt).toString(),
      budgetMs: Math.trunc(budget).toString(),
      credentials: this.request.credentialValues ?? [],
      headers,
      ...buildPropagationContext(this.request.headers),
      ...(this.resuming ? { resume: this.resuming } : {}),
    };
  }

  private startHeartbeat(): void {
    const cadence = this.session.budgets.heartbeatMs;
    if (cadence <= 0) return;
    // The heartbeat starts with the conversation, not with admission: a slow
    // asynchronous credential check must not look like a dead socket.
    this.heartbeatTimer = setInterval(() => {
      if (this.session.phase === 'terminal' || this.session.phase === 'closed') return;
      this.sendFrame({ v: 1, type: 'ping', nonce: crypto.randomUUID() });
    }, cadence);
  }

  private get maxFrameBytes(): number {
    return this.session.budgets.maxFrameBytes;
  }

  private get method(): string {
    return this.request.operationId ?? this.request.path;
  }

  /** Record the single terminal. Telling the provider and closing follow from it. */
  private terminate(error: unknown): void {
    this.session.fail(error);
    this.session.close();
  }

  /**
   * The session reached its terminal. Tell the provider once — but only when
   * the provider is not the peer that already terminated the conversation —
   * then close the socket.
   */
  private onSessionEnd(): void {
    this.release();
    if (this.socket?.readyState === WebSocket.OPEN && this.conversation.state !== 'terminal') {
      this.sendCancel(this.session.error instanceof ClientDeadlineError ? 'deadline_exceeded' : 'canceled');
    }
    try {
      this.socket?.close();
    } catch {
      // Already closed.
    }
  }

  /** The cancel frame bypasses the terminal guard: it *is* how the client terminates. */
  private sendCancel(code: 'canceled' | 'deadline_exceeded'): void {
    const frame: WebSocketServiceFrame = { v: 1, type: 'cancel', code };
    if (this.conversation.accept(frame, 'client-to-server').length > 0) return;
    this.write(frame);
  }

  private close(): void {
    this.release();
    try {
      this.socket?.close();
    } catch {
      // Already closed.
    }
    this.session.close();
  }

  private release(): void {
    clearInterval(this.heartbeatTimer);
    this.heartbeatTimer = undefined;
  }
}

/** A wire refusal, reported with the contract's own diagnostic code. */
function contractError(
  refusal: WebSocketDiagnostic | undefined,
  serviceId: string,
  method: string,
): ClientResponseContractError {
  const error = new ClientResponseContractError(
    refusal?.message ?? 'websocket frame is malformed or contains unsupported fields',
    serviceId,
    method,
  );
  return Object.assign(error, {
    contractCode: refusal?.code ?? 'client_contract.parse_error',
    contractField: refusal?.field ?? '',
  });
}

function buildPropagationContext(headers: Headers): Pick<WebSocketInitFrame, 'context'> {
  const context = {
    traceparent: headers.get('traceparent') ?? undefined,
    tracestate: headers.get('tracestate') ?? undefined,
    baggage: headers.get('baggage') ?? undefined,
    requestId: headers.get('X-Request-Id') ?? undefined,
  };
  const declared = Object.fromEntries(Object.entries(context).filter(([, value]) => value !== undefined));
  return Object.keys(declared).length > 0 ? { context: declared } : {};
}

async function messageText(value: unknown): Promise<string> {
  if (typeof value === 'string') return value;
  if (value instanceof Blob) return value.text();
  if (value instanceof ArrayBuffer) return new TextDecoder('utf-8', { fatal: true }).decode(value);
  if (ArrayBuffer.isView(value)) {
    return new TextDecoder('utf-8', { fatal: true }).decode(
      new Uint8Array(value.buffer, value.byteOffset, value.byteLength),
    );
  }
  throw new ClientResponseContractError('provider websocket frame has an unsupported representation');
}
