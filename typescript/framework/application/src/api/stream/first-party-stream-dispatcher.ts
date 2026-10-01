import type { ServerWebSocket } from 'bun';
import { useLogger } from '@putnami/runtime';
import type { ClientSecurityPolicy, ClientServiceContract, ClientTransportContract } from '../client-contract';
import type { WsCloseContext, WsDataType, WsMessageContext, WsOpenContext } from '../ws-context.type';
import type { HttpRequestContext, HttpRequestContextInternal } from '../../http/http-context.type';
import { HttpResponse } from '../../http/http-response';
import type { StreamEndpointDefinition } from '../route/stream-endpoint';
import { validateSchema } from '../route/validate';
import { MessageStream } from './message-stream';
import { type ResumeGrant, ResumeGrantStore, RESUME_BOUNDS } from './stream-resume';
import { type FirstPartyStreamError, projectStreamError } from './stream-error';
import {
  parseWebSocketServiceFrameV1,
  SERVICE_WEBSOCKET_SUBPROTOCOL,
  WebSocketConversationV1,
  type WebSocketDiagnostic,
  type WebSocketEncodedPayload,
  type WebSocketInitFrame,
  type WebSocketPayloadEncoding,
  WebSocketProtocolError,
  type WebSocketServiceFrame,
} from './websocket-protocol';

/**
 * The four session bounds plus the heartbeat cadence, resolved from
 * `resilience.stream` on the operation, then the document default, then the
 * framework floor. They are the provider half of the D0.7 budget contract.
 */
export interface FirstPartyStreamBudgets {
  /** Open to `ready` inclusive. Also bounds inactivity while admission waits on an async credential check. */
  readonly handshakeTimeoutMs: number;
  /** Maximum interval between two client frames after admission. */
  readonly idleTimeoutMs: number;
  /** Provider `ping` cadence. `0` means no heartbeat: an undeclared heartbeat hides a dead socket. */
  readonly heartbeatMs: number;
  /** Bound on one reassembled frame, never on a TCP frame. */
  readonly maxFrameBytes: number;
  /** Depth of the inbound queue the handler has not consumed. */
  readonly maxBufferedMessages: number;
}

/** Everything the admission machine needs about the operation the upgraded route serves. */
export interface FirstPartyStreamAdmission {
  readonly operationId: string;
  readonly contract: ClientServiceContract;
  readonly security: ClientSecurityPolicy;
  /**
   * The declared WebSocket transport, which owns the encoding and the resume
   * agreement. The conversation carries JSON or proto payloads, never the raw
   * octets only a provider-owned wire declares.
   */
  readonly transport: ClientTransportContract & { readonly encoding: WebSocketPayloadEncoding };
  readonly budgets: FirstPartyStreamBudgets;
}

/**
 * Follow the drain of the HTTP plugin that serves one conversation.
 *
 * A WebSocket is hijacked from the HTTP server, so `server.stop()` does not
 * end it: each conversation has to end itself, with its terminal, when the
 * serving plugin begins draining. The drain is read from the upgrade request,
 * never from the API plugin that registered the route: a scanned route folder
 * is loaded once per process and merged into every application instance that
 * scans it, so only the request knows which instance serves it.
 *
 * Runs `terminate` at once when the drain has already begun, and returns the
 * handle that stops following it otherwise.
 */
export function followServingDrain(
  context: HttpRequestContext | undefined,
  terminate: () => void,
): (() => void) | undefined {
  const drain = (context as HttpRequestContextInternal | undefined)?.__drain;
  if (!drain) return undefined;
  if (drain.aborted) {
    terminate();
    return undefined;
  }
  drain.addEventListener('abort', terminate, { once: true });
  return () => drain.removeEventListener('abort', terminate);
}

/**
 * Negotiate the first-party subprotocol for one upgrade.
 *
 * A client that offers nothing keeps the raw transport stream, which is what a
 * pre-contract WebSocket route has always been. A client that offers only
 * tokens this route cannot speak is refused at the upgrade rather than left to
 * fail its first frame: RFC 6455 §4.2.2 permits either, and refusing early
 * names the defect while the HTTP status is still available to carry it.
 */
export function negotiateFirstPartySubprotocol(offered: string | null): {
  readonly subprotocol?: string;
  readonly refused: boolean;
} {
  if (!offered) return { refused: false };
  const tokens = offered
    .split(',')
    .map((token) => token.trim())
    .filter((token) => token.length > 0);
  if (tokens.length === 0) return { refused: false };
  if (tokens.includes(SERVICE_WEBSOCKET_SUBPROTOCOL))
    return { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, refused: false };
  return { refused: true };
}

/**
 * Bridge a first-party stream endpoint to the Bun WebSocket lifecycle.
 *
 * Every rule of the wire comes from {@link WebSocketConversationV1}: this
 * dispatcher decides *what* the provider does, never *whether* a frame is legal
 * in the current state.
 */
export function resolveFirstPartyStreamHandlers(
  def: StreamEndpointDefinition,
  admission: FirstPartyStreamAdmission,
): {
  OPEN: (ctx: WsOpenContext) => void;
  MESSAGE: (ctx: WsMessageContext) => Promise<void>;
  CLOSE: (ctx: WsCloseContext) => void;
} {
  // One store per endpoint: a grant outlives the socket it was issued on, which
  // is the whole point, but never the endpoint that issued it.
  const resume = new ResumeGrantStore();
  return {
    OPEN: (ctx) => {
      const ws = socket(ctx);
      const state: FirstPartyStreamState = {
        conversation: new WebSocketConversationV1({
          stream: def.mode,
          encoding: admission.transport.encoding,
          resumeDeclared: admission.transport.websocket?.resume ?? false,
        }),
        admitted: false,
        terminal: false,
        serverSequence: 0n,
        resume,
        resumeBudget: RESUME_BOUNDS.budget,
        budgets: admission.budgets,
        handshakeTimer: setTimeout(
          () => failTyped(ws, ADMISSION_DEADLINE, 'admission deadline', 1008),
          admission.budgets.handshakeTimeoutMs,
        ),
      };
      ws.data.__firstPartyStream = state;
      state.release = followServingDrain(ws.data.__httpContext, () => failTyped(ws, SHUTTING_DOWN, 'shutting down'));
    },
    MESSAGE: async (ctx) => {
      const ws = socket(ctx);
      const state = ws.data.__firstPartyStream;
      if (!state || state.terminal) return;
      const raw = typeof ctx.message === 'string' ? ctx.message : new Uint8Array(ctx.message);
      const parsed = parseWebSocketServiceFrameV1(raw, state.budgets.maxFrameBytes);
      if (!parsed.frame) {
        failContract(ws, parsed.diagnostics[0]);
        return;
      }
      const refusal = state.conversation.accept(parsed.frame, 'client-to-server');
      if (refusal.length > 0) {
        failContract(ws, refusal[0]);
        return;
      }
      touchIdle(ws, state);
      if (parsed.frame.type === 'init') {
        await admitFirstPartyStream(ws, state, parsed.frame, def, admission);
        return;
      }
      dispatchAdmittedFrame(ws, state, parsed.frame, def);
    },
    CLOSE: (ctx) => closeFirstPartyState(socket(ctx).data.__firstPartyStream),
  };
}

// ---------------------------------------------------------------------------
// Admission
// ---------------------------------------------------------------------------

async function admitFirstPartyStream(
  ws: FirstPartySocket,
  state: FirstPartyStreamState,
  init: WebSocketInitFrame,
  def: StreamEndpointDefinition,
  admission: FirstPartyStreamAdmission,
): Promise<void> {
  // The conversation already refused a resume the transport does not declare.
  // This provider's own limits come next, so a refusal names the wire's rule
  // whenever the wire has one.
  const declared = admissionDiagnostic(init, admission);
  if (declared) {
    failContract(ws, declared);
    return;
  }
  // A continuation is redeemed before the security chain runs: a token that
  // buys nothing must not cost a credential check.
  const continuation = admitResume(state, init, def, admission);
  if ('refusal' in continuation) {
    failContract(ws, continuation.refusal);
    return;
  }
  const deadline = parseDeadline(init.deadlineUnixMs, init.budgetMs);
  if (deadline !== undefined && deadline <= Date.now()) {
    failTyped(ws, ADMISSION_DEADLINE, 'admission deadline', 1008);
    return;
  }
  let admitted: HttpRequestContext;
  let rejection: FirstPartyStreamError | undefined;
  try {
    admitted = admissionContext(ws, init, admission.contract);
    await runIdentityResolvers(ws, admitted);
    rejection = await runEndpointMiddleware(def, admitted);
    if (!rejection) validateStreamContext(admitted, def.schemas);
  } catch (error) {
    if (error instanceof WebSocketProtocolError) failContract(ws, error.diagnostic);
    else failTyped(ws, projectStreamError(error, def), 'stream rejected', 1008);
    return;
  }
  if (rejection) {
    failTyped(ws, rejection, 'stream rejected', 1008);
    return;
  }
  // The security chain may have awaited an external credential check. Re-read
  // the conversation before answering: a caller that cancelled, ran out of
  // budget, or an application that started draining must never see `ready`.
  if (state.terminal) return;
  if (deadline !== undefined && deadline <= Date.now()) {
    failTyped(ws, ADMISSION_DEADLINE, 'admission deadline');
    return;
  }
  clearTimeout(state.handshakeTimer);
  state.handshakeTimer = undefined;
  state.admitted = true;
  state.messageStream = new MessageStream<unknown>(admission.budgets.maxBufferedMessages);
  state.abort = new AbortController();
  if (deadline !== undefined) {
    state.deadlineTimer = setTimeout(
      () => failTyped(ws, ADMISSION_DEADLINE, 'deadline exceeded'),
      Math.max(1, deadline - Date.now()),
    );
  }
  // A continued stream carries on the sequence it left: the next message the
  // handler sends is `resumeAfter + 1`, so a consumer sees neither a gap nor a
  // value it already read.
  const resumed = init.resume !== undefined;
  if (resumed) state.serverSequence = continuation.after;
  // The token rotates on every ready frame: the one this connection presented
  // is spent, and only the new one can continue the stream after it.
  const issued = state.resume.issue(
    admission.operationId,
    init.clientId,
    continuation.after,
    resumableStream(def, admission) ? state.resumeBudget : 0,
  );
  state.grant = issued?.grant;
  if (
    !sendFrame(ws, state, {
      v: 1,
      type: 'ready',
      resumed,
      ...(issued ? { resumeToken: issued.token } : {}),
    })
  )
    return;
  startIdle(ws, state);
  startHeartbeat(ws, state);
  startFirstPartyHandler(ws, state, admitted, def, resumed ? continuation.after : undefined);
}

/**
 * Whether this endpoint's published transport states the stream can be
 * continued without a gap. Only a server stream ever can: continuing a duplex
 * conversation would replay the caller's own messages.
 */
function resumableStream(def: StreamEndpointDefinition, admission: FirstPartyStreamAdmission): boolean {
  return def.mode === 'server' && (admission.transport.websocket?.resume ?? false);
}

/**
 * Redeem the grant an init frame presents and report the position the stream
 * continues after. A fresh stream continues after nothing, which is `0`.
 */
function admitResume(
  state: FirstPartyStreamState,
  init: WebSocketInitFrame,
  def: StreamEndpointDefinition,
  admission: FirstPartyStreamAdmission,
): { readonly after: bigint } | { readonly refusal: WebSocketDiagnostic } {
  if (!init.resume) return { after: 0n };
  if (!resumableStream(def, admission)) {
    return {
      refusal: {
        code: 'client_contract.invalid_resilience',
        field: 'resume',
        message: 'selected websocket transport does not support resume',
      },
    };
  }
  let after: bigint;
  try {
    after = BigInt(init.resume.afterSequence);
  } catch {
    return {
      refusal: {
        code: 'client_contract.invalid_transport',
        field: 'resume.afterSequence',
        message: 'resume position is not a sequence this provider issued',
      },
    };
  }
  const redemption = state.resume.redeem(init.resume.token, admission.operationId, init.clientId, after);
  if ('refusal' in redemption) return redemption;
  state.resumeBudget = redemption.grant.budget - 1;
  return { after };
}

/**
 * The admission checks this provider owns: operation identity, the encodings it
 * can actually decode, declared credential profiles, one satisfied security
 * alternative, ordinary headers that would shadow a declared profile, and the
 * declared client allow-list.
 *
 * The security chain of the endpoint stays the authority on *authorization*;
 * this only rejects an init frame the operation could never have produced.
 */
function admissionDiagnostic(
  init: WebSocketInitFrame,
  admission: FirstPartyStreamAdmission,
): WebSocketDiagnostic | undefined {
  if (init.operationId !== admission.operationId) {
    return {
      code: 'client_contract.invalid_transport',
      field: 'operationId',
      message: 'init operation does not match the upgraded route',
    };
  }
  if (admission.transport.encoding !== 'json') {
    return {
      code: 'client_contract.invalid_transport',
      field: 'transport.encoding',
      message: `this provider cannot decode ${admission.transport.encoding} payloads for ${admission.operationId}`,
    };
  }
  const provided = new Set<string>();
  for (const credential of init.credentials) {
    provided.add(credential.profile);
    if (admission.contract.credentials[credential.profile] === undefined) {
      return {
        code: 'client_contract.unknown_profile',
        field: 'credentials',
        message: 'init references an undeclared credential profile',
      };
    }
  }
  const satisfied = admission.security.alternatives.some((alternative) => {
    const required = new Set(alternative.allOf.map((requirement) => requirement.profile));
    return required.size === provided.size && [...required].every((profile) => provided.has(profile));
  });
  if (!satisfied) {
    return {
      code: 'client_contract.invalid_security',
      field: 'credentials',
      message: 'init credentials do not satisfy one declared security alternative',
    };
  }
  const credentialHeaders = new Set(
    Object.values(admission.contract.credentials)
      .map((profile) => ('header' in profile ? profile.header.toLowerCase() : 'authorization'))
      .filter((name) => name.length > 0),
  );
  const shadowed = init.headers.findIndex((header) => credentialHeaders.has(header.name.toLowerCase()));
  if (shadowed >= 0) {
    return {
      code: 'client_contract.invalid_security',
      field: `headers[${shadowed}].name`,
      message: 'ordinary headers cannot override a declared credential profile',
    };
  }
  const clients = admission.security.authorization?.clients;
  if (clients && clients.length > 0 && !clients.includes(init.clientId)) {
    return {
      code: 'client_contract.invalid_security',
      field: 'clientId',
      message: 'client identity is not authorized for the operation',
    };
  }
  return undefined;
}

/**
 * Rebuild the request the security chain reads from the `init` frame.
 *
 * A conforming client carries no credential on the upgrade — a browser cannot
 * set one — so the chain has to run against this reconstruction rather than the
 * bare upgrade, or every first-party consumer would be refused.
 */
function admissionContext(
  ws: FirstPartySocket,
  init: WebSocketInitFrame,
  contract: ClientServiceContract,
): HttpRequestContext {
  const original = ws.data.__httpContext;
  if (!original) {
    throw new WebSocketProtocolError({
      code: 'client_contract.invalid_transport',
      field: 'init',
      message: 'websocket upgrade context is missing',
    });
  }
  const headers = new Headers();
  for (const header of init.headers) for (const value of header.values) headers.append(header.name, value);
  headers.set('X-Client-Id', init.clientId);
  if (init.context?.traceparent) headers.set('traceparent', init.context.traceparent);
  if (init.context?.tracestate) headers.set('tracestate', init.context.tracestate);
  if (init.context?.baggage) headers.set('baggage', init.context.baggage);
  if (init.context?.requestId) headers.set('X-Request-Id', init.context.requestId);
  for (const credential of init.credentials) {
    const profile = contract.credentials[credential.profile];
    if (!profile) {
      throw new WebSocketProtocolError({
        code: 'client_contract.unknown_profile',
        field: 'credentials',
        message: 'init references an undeclared credential profile',
      });
    }
    const name =
      profile.kind === 'service-token' || profile.kind === 'forwarded-user-token' ? 'Authorization' : profile.header;
    headers.set(name, credential.value);
  }
  const req = new Request(original.req.url, { method: original.req.method, headers });
  return { ...original, req, headers, user: undefined };
}

async function runIdentityResolvers(ws: FirstPartySocket, context: HttpRequestContext): Promise<void> {
  for (const resolver of ws.data.__identityResolvers ?? []) {
    // biome-ignore lint/performance/noAwaitInLoops: identity resolvers are ordered and first-win
    await resolver(context, async () => undefined);
  }
}

/**
 * Run the endpoint's own middleware chain and return the refusal it produced,
 * projected into the shared envelope. `undefined` means the chain accepted.
 */
async function runEndpointMiddleware(
  def: StreamEndpointDefinition,
  context: HttpRequestContext,
): Promise<FirstPartyStreamError | undefined> {
  const accepted = new HttpResponse(undefined, { status: 204 });
  const chain = [...(def.middleware ?? [])].reduceRight<() => Promise<HttpResponse | undefined>>(
    (next, middleware) => () => middleware(context, next),
    async () => accepted,
  );
  const result = await chain();
  if (result === accepted) return undefined;
  const status = result?.status ?? (context.user ? 403 : 401);
  return {
    status,
    code: status === 403 ? 'forbidden' : 'unauthorized',
    error: status === 403 ? 'Forbidden' : 'Unauthorized',
    message: status === 403 ? 'Forbidden' : 'Unauthorized',
  };
}

function validateStreamContext(context: HttpRequestContext, schemas?: StreamEndpointDefinition['schemas']): void {
  if (schemas?.params) {
    context.params = validateSchema(schemas.params, context.params ?? {}, { coerce: true, label: 'params' }) as Record<
      string,
      string
    >;
  }
  if (schemas?.query) {
    const query = validateSchema(schemas.query, context.queryParams(), { coerce: true, label: 'query' });
    context.queryParams = () => query as Record<string, string>;
  }
}

// ---------------------------------------------------------------------------
// Admitted conversation
// ---------------------------------------------------------------------------

function dispatchAdmittedFrame(
  ws: FirstPartySocket,
  state: FirstPartyStreamState,
  frame: WebSocketServiceFrame,
  def: StreamEndpointDefinition,
): void {
  switch (frame.type) {
    case 'ping':
      sendFrame(ws, state, { v: 1, type: 'pong', nonce: frame.nonce });
      return;
    case 'pong':
      return;
    case 'cancel':
      state.terminal = true;
      state.abort?.abort();
      closeSocket(ws, state, 1000, 'canceled');
      return;
    case 'half-close':
      state.messageStream?.close();
      return;
    case 'message': {
      let value: unknown;
      try {
        value = def.schemas?.body
          ? validateSchema(def.schemas.body, payloadValue(frame.payload), { label: 'message' })
          : payloadValue(frame.payload);
      } catch (error) {
        failTyped(ws, projectStreamError(error, def), 'stream rejected');
        return;
      }
      state.messageStream?.push(value);
      // A full queue is a terminal back-pressure error, never a silent drop:
      // Bun's server socket exposes no read pause, so the bound is enforced by
      // ending the conversation rather than by slowing the peer.
      if ((state.messageStream?.droppedCount ?? 0) > 0) failTyped(ws, BACKPRESSURE, 'back-pressure');
      return;
    }
    default:
      return;
  }
}

function startFirstPartyHandler(
  ws: FirstPartySocket,
  state: FirstPartyStreamState,
  context: HttpRequestContext,
  def: StreamEndpointDefinition,
  resumeFrom?: bigint,
): void {
  const streamContext = {
    ...context,
    signal: state.abort?.signal,
    // A continued stream tells its handler where the consumer left, so it
    // produces what has not been received rather than the whole stream. A
    // fresh stream carries nothing here, and a handler that ignores it always
    // produces everything.
    ...(resumeFrom === undefined ? {} : { resumeFrom }),
    messages: () => state.messageStream as MessageStream<unknown>,
    send: (input: unknown) => {
      if (state.terminal) return;
      const value = def.schemas?.returns ? validateSchema(def.schemas.returns, input, { label: 'message' }) : input;
      const sequence = state.serverSequence + 1n;
      if (
        sendFrame(ws, state, {
          v: 1,
          type: 'message',
          sequence: sequence.toString(),
          payload: { encoding: 'json', value },
        })
      ) {
        state.serverSequence = sequence;
        // The grant buys the position this provider has actually reached, so a
        // later continuation cannot claim more than was delivered.
        if (state.grant) state.grant.cursor = sequence;
      }
    },
  };
  Promise.resolve(def.handler(streamContext)).then(
    (result) => finishHandler(ws, state, result, def),
    (error) => failHandler(ws, state, error, def, context.path()),
  );
}

function finishHandler(
  ws: FirstPartySocket,
  state: FirstPartyStreamState,
  result: unknown,
  def: StreamEndpointDefinition,
): void {
  if (state.terminal) return;
  state.terminal = true;
  // A server stream delivers its values in message frames, so its result never
  // carries a payload. A client or bidirectional stream must carry one: it is
  // the single value the caller's `result()` awaits.
  const payload =
    def.mode === 'server'
      ? undefined
      : {
          encoding: 'json' as const,
          value: def.schemas?.returns ? validateSchema(def.schemas.returns, result, { label: 'result' }) : result,
        };
  writeTerminal(ws, state, { v: 1, type: 'result', ...(payload ? { payload } : {}) }, 1000, 'complete');
}

function failHandler(
  ws: FirstPartySocket,
  state: FirstPartyStreamState,
  error: unknown,
  def: StreamEndpointDefinition,
  path: string,
): void {
  useLogger('ws').error('stream handler error', toError(error), { path });
  if (state.terminal) return;
  state.terminal = true;
  writeTerminal(
    ws,
    state,
    { v: 1, type: 'error', error: webSocketErrorMember(projectStreamError(error, def)) } as WebSocketServiceFrame,
    1011,
    'handler error',
  );
}

// ---------------------------------------------------------------------------
// Frames out
// ---------------------------------------------------------------------------

/**
 * Write one frame after the wire has read it back.
 *
 * Every outgoing frame is re-parsed by the strict parser and accepted by the
 * conversation before a byte leaves. Without that, a provider could write bytes
 * no conforming client accepts, and the defect would surface as an unexplained
 * client-side refusal instead of a provider fault.
 */
function sendFrame(ws: FirstPartySocket, state: FirstPartyStreamState, frame: WebSocketServiceFrame): boolean {
  const data = JSON.stringify(frame);
  const parsed = parseWebSocketServiceFrameV1(data, state.budgets.maxFrameBytes);
  if (!parsed.frame) {
    failLocal(ws, state, parsed.diagnostics[0]);
    return false;
  }
  const refusal = state.conversation.accept(parsed.frame, 'server-to-client');
  if (refusal.length > 0) {
    failLocal(ws, state, refusal[0]);
    return false;
  }
  ws.send(data);
  return true;
}

/** Write the single terminal frame, then close. */
function writeTerminal(
  ws: FirstPartySocket,
  state: FirstPartyStreamState,
  frame: WebSocketServiceFrame,
  code: number,
  reason: string,
): void {
  const data = JSON.stringify(frame);
  const parsed = parseWebSocketServiceFrameV1(data, state.budgets.maxFrameBytes);
  if (parsed.frame && state.conversation.accept(parsed.frame, 'server-to-client').length === 0) {
    trySend(ws, data);
    closeSocket(ws, state, code, reason);
    return;
  }
  // The frame this provider composed is not one the wire admits. Report the
  // contract's own diagnostic rather than writing bytes a client must refuse.
  trySend(
    ws,
    JSON.stringify({ v: 1, type: 'error', error: webSocketErrorMember(contractEnvelope(parsed.diagnostics[0])) }),
  );
  closeSocket(ws, state, 1011, 'contract violation');
}

/** A frame this provider composed that the wire refuses is a local fault, not the peer's. */
function failLocal(ws: FirstPartySocket, state: FirstPartyStreamState, refusal?: WebSocketDiagnostic): void {
  if (state.terminal) return;
  state.terminal = true;
  trySend(ws, JSON.stringify({ v: 1, type: 'error', error: webSocketErrorMember(contractEnvelope(refusal)) }));
  closeSocket(ws, state, 1011, 'contract violation');
}

/** Refuse the peer's frame with the contract's own diagnostic code. */
function failContract(ws: FirstPartySocket, refusal?: WebSocketDiagnostic): void {
  failTyped(ws, contractEnvelope(refusal), 'stream rejected', 1008);
}

/**
 * End the conversation with one typed terminal error.
 *
 * The close reason is a fixed framework string: a reason is visible to
 * intermediaries, so it never carries a message, a detail body or a credential.
 */
function failTyped(ws: FirstPartySocket, error: FirstPartyStreamError, reason: string, closeCode = 1011): void {
  const state = ws.data.__firstPartyStream;
  if (!state || state.terminal) return;
  state.terminal = true;
  state.abort?.abort();
  trySend(ws, JSON.stringify({ v: 1, type: 'error', error: webSocketErrorMember(error) }));
  closeSocket(ws, state, closeCode, reason);
}

/**
 * The `error` member of a WebSocket error frame.
 *
 * The wire's closed field set has no status phrase, so the shared envelope's
 * `error` stays behind: a frame carrying it is one no conforming peer accepts.
 * Everything a consumer decodes — status, stable code, message, declared
 * details — travels unchanged.
 */
function webSocketErrorMember(error: FirstPartyStreamError): Record<string, unknown> {
  return {
    status: error.status,
    code: error.code,
    message: error.message,
    ...(error.details === undefined ? {} : { details: error.details }),
  };
}

function contractEnvelope(refusal?: WebSocketDiagnostic): FirstPartyStreamError {
  return {
    status: 400,
    code: refusal?.code ?? 'client_contract.parse_error',
    error: 'Bad Request',
    message: refusal?.message ?? 'websocket frame is malformed or contains unsupported fields',
  };
}

const ADMISSION_DEADLINE: FirstPartyStreamError = {
  status: 408,
  code: 'client.deadline',
  error: 'Request Timeout',
  message: 'stream budget elapsed before admission',
};

const SHUTTING_DOWN: FirstPartyStreamError = {
  status: 503,
  code: 'http.service_unavailable',
  error: 'Service Unavailable',
  message: 'provider is shutting down',
};

const BACKPRESSURE: FirstPartyStreamError = {
  status: 429,
  code: 'http.too_many_requests',
  error: 'Too Many Requests',
  message: 'inbound stream queue exceeded its declared depth',
};

function trySend(ws: FirstPartySocket, data: string): void {
  try {
    ws.send(data);
  } catch {
    // The peer is already gone; the terminal is recorded either way.
  }
}

/**
 * Close the socket on the next turn of the event loop.
 *
 * Bun discards payloads still queued on a socket when `close()` runs in the
 * same tick as the `send()` that queued them. A handler that fails before its
 * first await — a declared `NotFoundException`, a refused admission — writes
 * its terminal frame and closes in one tick, and the peer would then see only
 * the close code: a typed terminal would silently become
 * `provider closed the websocket without a terminal frame`. Yielding once lets
 * the frame leave before the close frame does. `flush()` does not help; the
 * yield is what the event loop needs.
 */
function closeSocket(ws: FirstPartySocket, state: FirstPartyStreamState, code: number, reason: string): void {
  releaseTimers(state);
  setTimeout(() => {
    try {
      ws.close(code, reason);
    } catch {
      // Already closed.
    }
  }, 0);
}

// ---------------------------------------------------------------------------
// Budgets
// ---------------------------------------------------------------------------

function startIdle(ws: FirstPartySocket, state: FirstPartyStreamState): void {
  if (state.budgets.idleTimeoutMs <= 0) return;
  state.idleTimer = setTimeout(() => failTyped(ws, IDLE_DEADLINE, 'idle deadline'), state.budgets.idleTimeoutMs);
}

function touchIdle(ws: FirstPartySocket, state: FirstPartyStreamState): void {
  if (!state.admitted || state.budgets.idleTimeoutMs <= 0) return;
  clearTimeout(state.idleTimer);
  startIdle(ws, state);
}

const IDLE_DEADLINE: FirstPartyStreamError = {
  status: 408,
  code: 'client.deadline',
  error: 'Request Timeout',
  message: 'stream was idle past its declared bound',
};

function startHeartbeat(ws: FirstPartySocket, state: FirstPartyStreamState): void {
  if (state.budgets.heartbeatMs <= 0) return;
  state.heartbeatTimer = setInterval(() => {
    if (state.terminal) return;
    sendFrame(ws, state, { v: 1, type: 'ping', nonce: crypto.randomUUID() });
  }, state.budgets.heartbeatMs);
}

/**
 * The caller's declared budget: the earlier of the absolute deadline and the
 * relative budget. `0` in either member means the caller declared none.
 */
function parseDeadline(deadline: string, budget: string): number | undefined {
  const now = Date.now();
  const candidates = [
    deadline === '0' ? undefined : Number(deadline),
    budget === '0' ? undefined : now + Number(budget),
  ].filter((value): value is number => value !== undefined && Number.isSafeInteger(value));
  return candidates.length ? Math.min(...candidates) : undefined;
}

function releaseTimers(state: FirstPartyStreamState): void {
  clearTimeout(state.handshakeTimer);
  clearTimeout(state.deadlineTimer);
  clearTimeout(state.idleTimer);
  clearInterval(state.heartbeatTimer);
  state.release?.();
  state.release = undefined;
}

function closeFirstPartyState(state: FirstPartyStreamState | undefined): void {
  if (!state) return;
  state.terminal = true;
  releaseTimers(state);
  state.abort?.abort();
  state.messageStream?.close();
}

// ---------------------------------------------------------------------------
// Shapes
// ---------------------------------------------------------------------------

/** The decoded value of a JSON payload. A proto payload never reaches here: it is refused at admission. */
function payloadValue(payload: WebSocketEncodedPayload): unknown {
  return payload.encoding === 'json' ? payload.value : undefined;
}

function socket(context: WsOpenContext | WsMessageContext | WsCloseContext): FirstPartySocket {
  return context.ws as ServerWebSocket<FirstPartyWsData>;
}

type FirstPartySocket = ServerWebSocket<FirstPartyWsData>;
type FirstPartyWsData = WsDataType & { __firstPartyStream?: FirstPartyStreamState };

interface FirstPartyStreamState {
  conversation: WebSocketConversationV1;
  admitted: boolean;
  terminal: boolean;
  serverSequence: bigint;
  /** The endpoint's live continuation grants. Empty unless resume is declared. */
  resume: ResumeGrantStore;
  /**
   * The number of continuations this stream still allows. It is decremented by
   * every redeemed grant, so a stream that keeps breaking stops being
   * resumable instead of living forever.
   */
  resumeBudget: number;
  /**
   * The live continuation grant of this conversation. Every message this
   * provider sends moves the position it buys.
   */
  grant?: ResumeGrant;
  budgets: FirstPartyStreamBudgets;
  handshakeTimer?: ReturnType<typeof setTimeout>;
  deadlineTimer?: ReturnType<typeof setTimeout>;
  idleTimer?: ReturnType<typeof setTimeout>;
  heartbeatTimer?: ReturnType<typeof setInterval>;
  messageStream?: MessageStream<unknown>;
  abort?: AbortController;
  release?: () => void;
}

function toError(value: unknown): Error {
  return value instanceof Error ? value : new Error(String(value), { cause: value });
}

// ---------------------------------------------------------------------------
// Admission metadata
// ---------------------------------------------------------------------------

/** Framework floors, used only where the provider declaration is silent. */
const BUDGET_FLOORS = Object.freeze({
  handshakeTimeoutMs: 10_000,
  idleTimeoutMs: 30_000,
  /** No heartbeat by default: one nobody declared hides a dead socket instead of revealing it. */
  heartbeatMs: 0,
  maxFrameBytes: 1024 * 1024,
  maxBufferedMessages: 16,
});

/**
 * Resolve what the admission machine needs from the provider declaration.
 *
 * The security policy is derived by the same rule the published document uses:
 * the operation's declared alternatives when it has them, the explicit
 * anonymous branch when the route is not secured. A secured route with no
 * declared alternative has no admissible credential shape at all, so it is
 * refused here rather than admitted anonymously.
 */
export function resolveFirstPartyStreamAdmission(options: {
  readonly operationId: string;
  readonly path: string;
  readonly contract: ClientServiceContract;
  readonly definition: StreamEndpointDefinition;
}): FirstPartyStreamAdmission {
  const policy = options.definition.meta?.client;
  const security: ClientSecurityPolicy = policy?.security ?? { alternatives: [{ allOf: [] }] };
  const stream = { ...options.contract.defaults?.resilience?.stream, ...policy?.resilience?.stream };
  const attemptTimeoutMs =
    policy?.resilience?.attemptTimeoutMs ?? options.contract.defaults?.resilience?.attemptTimeoutMs;
  return {
    operationId: options.operationId,
    contract: options.contract,
    security,
    transport: {
      protocol: 'websocket',
      path: options.path,
      encoding: 'json',
      // Resume is declared on the endpoint and only ever honored for a server
      // stream: continuing a duplex conversation would replay the caller's own
      // messages.
      websocket: {
        subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL,
        resume: policy?.resume === true && options.definition.mode === 'server',
      },
    },
    budgets: {
      handshakeTimeoutMs: positive(stream.handshakeTimeoutMs ?? attemptTimeoutMs, BUDGET_FLOORS.handshakeTimeoutMs),
      idleTimeoutMs: positive(stream.idleTimeoutMs, BUDGET_FLOORS.idleTimeoutMs),
      heartbeatMs: positive(stream.heartbeatMs, BUDGET_FLOORS.heartbeatMs),
      maxFrameBytes: positive(stream.maxFrameBytes, BUDGET_FLOORS.maxFrameBytes),
      maxBufferedMessages: positive(stream.maxBufferedMessages, BUDGET_FLOORS.maxBufferedMessages),
    },
  };
}

function positive(value: number | undefined, floor: number): number {
  return value !== undefined && Number.isSafeInteger(value) && value > 0 ? value : floor;
}
