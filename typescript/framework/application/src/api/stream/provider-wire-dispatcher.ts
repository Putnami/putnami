import type { ServerWebSocket } from 'bun';
import { BadRequestException, HttpException, useLogger } from '@putnami/runtime';
import type { HttpRequestContext } from '../../http/http-context.type';
import type { HttpResponse } from '../../http/http-response';
import { upgradeToWebSocket } from '../api-ws.utils';
import type { ClientOperationPolicy, ClientServiceContract } from '../client-contract';
import type { ProviderWire } from '../route/byte-stream';
import type { StreamEndpointDefinition } from '../route/stream-endpoint';
import { validateSchema } from '../route/validate';
import type { WsCloseContext, WsDataType, WsMessageContext, WsOpenContext } from '../ws-context.type';
import { followServingDrain } from './first-party-stream-dispatcher';
import { MessageStream } from './message-stream';

/**
 * The bounds of one provider-owned socket, read from `resilience.stream` on the
 * operation, then the document default, then the framework floor. An idle bound
 * or a heartbeat nobody declared is not applied: the first-party conversation
 * follows the same rule, and a heartbeat nobody asked for hides a dead socket.
 */
export interface ProviderWireBudgets {
  /** Maximum interval between two client messages. `0` means unbounded. */
  readonly idleTimeoutMs: number;
  /** RFC 6455 ping cadence. `0` means no heartbeat. */
  readonly heartbeatMs: number;
  /** Bound on one message, both directions. Outgoing octets are split under it. */
  readonly maxFrameBytes: number;
  /** Depth of the inbound queue the handler has not consumed. */
  readonly maxBufferedMessages: number;
}

const FLOORS = Object.freeze({ maxFrameBytes: 1024 * 1024, maxBufferedMessages: 16 });

/** RFC 6455 section 7.4.1 close codes this dispatcher sends. */
const CLOSE = Object.freeze({
  normal: 1000,
  goingAway: 1001,
  unsupportedData: 1003,
  invalidPayload: 1007,
  policyViolation: 1008,
  tooBig: 1009,
  internalError: 1011,
});

/** Resolve the declared bounds of one provider-owned wire. */
export function resolveProviderWireBudgets(
  contract: ClientServiceContract | undefined,
  policy: ClientOperationPolicy | undefined,
): ProviderWireBudgets {
  const stream = { ...contract?.defaults?.resilience?.stream, ...policy?.resilience?.stream };
  return {
    idleTimeoutMs: positive(stream.idleTimeoutMs, 0),
    heartbeatMs: positive(stream.heartbeatMs, 0),
    maxFrameBytes: positive(stream.maxFrameBytes, FLOORS.maxFrameBytes),
    maxBufferedMessages: positive(stream.maxBufferedMessages, FLOORS.maxBufferedMessages),
  };
}

/**
 * The upgrade handler of a provider-owned wire. It runs behind the endpoint's
 * middleware, so by the time it runs the upgrade request is admitted; it then
 * validates params and query exactly as SSE does, and upgrades only when the
 * client offered exactly the declared token — or none when none is declared.
 */
export function createProviderWireUpgrade(
  def: StreamEndpointDefinition,
  wire: ProviderWire,
): (context: HttpRequestContext) => HttpResponse {
  return (context) => {
    if (!offersTheDeclaredSubprotocol(context.req.headers.get('sec-websocket-protocol'), wire.subprotocol)) {
      throw new BadRequestException('requested websocket subprotocol is not the one this endpoint speaks');
    }
    if (def.schemas?.params) {
      context.params = validateSchema(def.schemas.params, context.params ?? {}, {
        coerce: true,
        label: 'params',
      }) as Record<string, string>;
    }
    if (def.schemas?.query) {
      const query = validateSchema(def.schemas.query, context.queryParams(), { coerce: true, label: 'query' });
      context.queryParams = () => query as Record<string, string>;
    }
    return upgradeToWebSocket(context, wire.subprotocol ? { subprotocol: wire.subprotocol } : undefined);
  };
}

/**
 * Whether an offer fits a route that speaks one declared token, or none.
 * RFC 6455 section 4.2.2 lets a server select one offered token; a route with
 * no token can select nothing, so any offer is a vocabulary it does not speak.
 */
export function offersTheDeclaredSubprotocol(offered: string | null, declared: string | undefined): boolean {
  const tokens = (offered ?? '')
    .split(',')
    .map((token) => token.trim())
    .filter((token) => token.length > 0);
  return declared === undefined ? tokens.length === 0 : tokens.includes(declared);
}

/**
 * Bridge a provider-owned wire to the Bun WebSocket lifecycle. The dispatcher
 * owns the socket — bounds, heartbeat, shutdown and close codes — and nothing
 * of the vocabulary: a byte stream hands octets to the handler, a typed wire
 * hands it one validated JSON value per text message.
 */
export function resolveProviderWireHandlers(
  def: StreamEndpointDefinition,
  wire: ProviderWire,
  budgets: ProviderWireBudgets,
): {
  OPEN: (ctx: WsOpenContext) => void;
  MESSAGE: (ctx: WsMessageContext) => void;
  CLOSE: (ctx: WsCloseContext) => void;
} {
  return {
    OPEN: (ctx) => {
      const ws = socket(ctx);
      const context = ws.data.__httpContext;
      const state: ProviderWireState = {
        messages: new MessageStream<unknown>(budgets.maxBufferedMessages),
        abort: new AbortController(),
        terminal: false,
      };
      ws.data.__providerWire = state;
      if (!context) {
        end(ws, state, CLOSE.internalError, 'upgrade context is missing');
        return;
      }
      state.release = followServingDrain(context, () => end(ws, state, CLOSE.goingAway, 'server shutting down'));
      if (state.terminal) return;
      startIdle(ws, state, budgets);
      if (budgets.heartbeatMs > 0) {
        state.heartbeatTimer = setInterval(() => {
          if (!state.terminal) ws.ping();
        }, budgets.heartbeatMs);
      }
      const streamContext = {
        ...context,
        signal: state.abort.signal,
        messages: () => state.messages,
        send: (value: unknown) => send(ws, state, def, wire, budgets, value),
      };
      Promise.resolve()
        .then(() => def.handler(streamContext))
        .then(
          () => end(ws, state, CLOSE.normal, 'stream complete'),
          (error) => {
            if (state.terminal) return;
            useLogger('ws').error('provider websocket stream handler error', toError(error), {
              path: context.path(),
            });
            end(
              ws,
              state,
              closeCodeForStatus(error instanceof HttpException ? error.getStatus() : 500),
              'stream failed',
            );
          },
        );
    },
    MESSAGE: (ctx) => {
      const ws = socket(ctx);
      const state = ws.data.__providerWire;
      if (!state || state.terminal) return;
      touchIdle(ws, state, budgets);
      const message = ctx.message as string | Uint8Array;
      const text = typeof message === 'string';
      if (text === wire.bytes) {
        end(ws, state, CLOSE.unsupportedData, 'unsupported message type');
        return;
      }
      const size = text ? Buffer.byteLength(message) : message.byteLength;
      if (size > budgets.maxFrameBytes) {
        end(ws, state, CLOSE.tooBig, 'message too big');
        return;
      }
      if (wire.bytes) {
        const octets = message as Uint8Array;
        state.messages.push(
          new Uint8Array(octets.buffer.slice(octets.byteOffset, octets.byteOffset + octets.byteLength)),
        );
      } else {
        let value: unknown;
        try {
          value = JSON.parse(message as string);
          if (def.schemas?.body) value = validateSchema(def.schemas.body, value, { label: 'message' });
        } catch {
          end(ws, state, CLOSE.invalidPayload, 'invalid frame');
          return;
        }
        state.messages.push(value);
      }
      // Bun's server socket exposes no read pause, so a full queue ends the
      // stream instead of silently dropping what the client sent.
      if (state.messages.droppedCount > 0) end(ws, state, CLOSE.policyViolation, 'back-pressure');
    },
    CLOSE: (ctx) => {
      const state = socket(ctx).data.__providerWire;
      if (!state) return;
      // A normal client close ends the client's direction and nothing else: a
      // byte handler reads the end of its messages. Any other close is the
      // stream failing, and the handler's signal says so.
      if (ctx.code !== CLOSE.normal) state.abort.abort();
      state.terminal = true;
      state.messages.close();
      release(state);
    },
  };
}

/** Write one handler value. */
function send(
  ws: ProviderWireSocket,
  state: ProviderWireState,
  def: StreamEndpointDefinition,
  wire: ProviderWire,
  budgets: ProviderWireBudgets,
  value: unknown,
): void {
  if (state.terminal) return;
  if (wire.bytes) {
    const octets = toOctets(value);
    for (let offset = 0; offset < octets.byteLength; offset += budgets.maxFrameBytes) {
      ws.sendBinary(octets.subarray(offset, Math.min(offset + budgets.maxFrameBytes, octets.byteLength)));
    }
    return;
  }
  const validated = def.schemas?.returns ? validateSchema(def.schemas.returns, value, { label: 'message' }) : value;
  const data = JSON.stringify(validated);
  if (Buffer.byteLength(data) > budgets.maxFrameBytes) {
    // A typed frame is one value; splitting it would change its meaning.
    throw new Error('provider websocket frame exceeds the declared frame bound');
  }
  ws.sendText(data);
}

function toOctets(value: unknown): Uint8Array {
  if (value instanceof Uint8Array) return value;
  if (value instanceof ArrayBuffer) return new Uint8Array(value);
  if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  throw new TypeError('a byte stream sends Uint8Array or ArrayBuffer values only');
}

/**
 * End the socket with the code that says how it ended. The close runs on the
 * next turn of the event loop: Bun discards payloads still queued on a socket
 * when `close()` runs in the same tick as the `send()` that queued them.
 */
function end(ws: ProviderWireSocket, state: ProviderWireState, code: number, reason: string): void {
  if (state.terminal) return;
  state.terminal = true;
  if (code !== CLOSE.normal) state.abort.abort();
  state.messages.close();
  release(state);
  setTimeout(() => {
    try {
      ws.close(code, reason);
    } catch {
      // Already closed.
    }
  }, 0);
}

function startIdle(ws: ProviderWireSocket, state: ProviderWireState, budgets: ProviderWireBudgets): void {
  if (budgets.idleTimeoutMs <= 0) return;
  state.idleTimer = setTimeout(() => end(ws, state, CLOSE.policyViolation, 'idle timeout'), budgets.idleTimeoutMs);
}

function touchIdle(ws: ProviderWireSocket, state: ProviderWireState, budgets: ProviderWireBudgets): void {
  if (budgets.idleTimeoutMs <= 0) return;
  clearTimeout(state.idleTimer);
  startIdle(ws, state, budgets);
}

function release(state: ProviderWireState): void {
  clearTimeout(state.idleTimer);
  clearInterval(state.heartbeatTimer);
  state.release?.();
  state.release = undefined;
}

/** Map a handler's failure status onto the RFC 6455 vocabulary. */
function closeCodeForStatus(status: number): number {
  if (status === 503) return CLOSE.goingAway;
  if (status >= 400 && status < 500) return CLOSE.policyViolation;
  return CLOSE.internalError;
}

function positive(value: number | undefined, floor: number): number {
  return value !== undefined && Number.isSafeInteger(value) && value > 0 ? value : floor;
}

function socket(context: WsOpenContext | WsMessageContext | WsCloseContext): ProviderWireSocket {
  return context.ws as ServerWebSocket<ProviderWireData>;
}

function toError(value: unknown): Error {
  return value instanceof Error ? value : new Error(String(value), { cause: value });
}

type ProviderWireSocket = ServerWebSocket<ProviderWireData>;
type ProviderWireData = WsDataType & { __providerWire?: ProviderWireState };

interface ProviderWireState {
  messages: MessageStream<unknown>;
  abort: AbortController;
  terminal: boolean;
  idleTimer?: ReturnType<typeof setTimeout>;
  heartbeatTimer?: ReturnType<typeof setInterval>;
  release?: () => void;
}
