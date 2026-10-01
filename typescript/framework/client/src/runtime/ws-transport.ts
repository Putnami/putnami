import type { DuplexStream, StreamObserver } from './stream.type';
import { assertWebSocketUrl } from './url';

/** Maximum number of messages buffered before WebSocket is open. */
const MAX_PENDING = 512;

/** Timeout for WebSocket handshake in ms. */
const WS_CONNECT_TIMEOUT_MS = 15_000;

/** Timeout for idle connections (no messages received) in ms. */
const WS_IDLE_TIMEOUT_MS = 60_000;

/** RFC 6455 token pattern: printable ASCII, no separators. */
const WS_TOKEN_RE = /^[a-zA-Z0-9\-_.~]{1,128}$/;

/**
 * WebSocket-based streaming transport.
 *
 * Carries client- and bidi-streaming RPCs (Connect over HTTP/1.1 cannot), and
 * is the fallback for server-streaming when the Connect route answers gRPC
 * status 12 `UNIMPLEMENTED`. WebSocket is universally supported.
 *
 * Protocol:
 * - Connect to `ws(s)://host/path`
 * - Send auth as first JSON frame after connection (not in sub-protocols)
 * - Send initial request body as JSON
 * - Server sends JSON messages (one per WS frame)
 * - Server closes connection when stream ends
 * - Client can cancel by closing the connection
 */
export class WebSocketTransport {
  private readonly wsUrl: string;

  constructor(baseUrl: string) {
    // Reject non-http(s)/ws(s) schemes before building the URL (SSRF guard).
    const validated = assertWebSocketUrl(baseUrl, 'WebSocketTransport baseUrl');
    const url = validated.endsWith('/') ? validated.slice(0, -1) : validated;
    this.wsUrl = url.replace(/^http/, 'ws');
  }

  /**
   * Open a server-streaming connection.
   * Sends the request body once, then listens for messages.
   *
   * `headers` may be a Promise — auth/context interceptors resolve headers
   * asynchronously (e.g. M2M token fetch), so socket construction is deferred
   * until they are available (trace headers become WS sub-protocols).
   */
  stream<T>(path: string, body?: unknown, headers?: Headers | Promise<Headers>): StreamObserver<T> {
    const conn = this.setupWebSocket(path, headers, (ws) => {
      ws.send(JSON.stringify(body ?? {}));
    });

    return {
      onMessage(handler: (data: T) => void) {
        conn.handlers.message = handler as (data: unknown) => void;
      },
      onError(handler: (error: Error) => void) {
        conn.handlers.error = handler;
      },
      onComplete(handler: () => void) {
        conn.handlers.complete = handler;
      },
      cancel() {
        conn.cancel();
      },
    };
  }

  /**
   * Open a bidirectional streaming connection.
   * Client can send messages and receive messages concurrently.
   *
   * See {@link stream} for why `headers` may be a Promise.
   */
  streamDuplex<TIn, TOut>(path: string, body?: unknown, headers?: Headers | Promise<Headers>): DuplexStream<TIn, TOut> {
    let ready = false;
    const pending: string[] = [];

    const conn = this.setupWebSocket(path, headers, (ws) => {
      if (body !== undefined) {
        ws.send(JSON.stringify(body));
      }
      ready = true;
      for (const msg of pending) {
        ws.send(msg);
      }
      pending.length = 0;
    });

    return {
      onMessage(handler: (data: TOut) => void) {
        conn.handlers.message = handler as (data: unknown) => void;
      },
      onError(handler: (error: Error) => void) {
        conn.handlers.error = handler;
      },
      onComplete(handler: () => void) {
        conn.handlers.complete = handler;
      },
      send(data: TIn) {
        const msg = JSON.stringify(data);
        const ws = conn.socket();
        if (ready && ws) {
          ws.send(msg);
        } else {
          if (pending.length >= MAX_PENDING) {
            pending.length = 0;
            conn.handlers.error?.(new Error('WebSocket send queue overflow'));
            conn.cancel();
            return;
          }
          pending.push(msg);
        }
      },
      end() {
        conn.socket()?.send(JSON.stringify({ __end: true }));
      },
      cancel() {
        conn.cancel();
      },
    };
  }

  /**
   * Shared WebSocket setup: connection, timeout, event listeners, cleanup.
   */
  private setupWebSocket(
    path: string,
    headers: Headers | Promise<Headers> | undefined,
    onReady: (ws: WebSocket) => void,
  ): WsConnection {
    const url = `${this.wsUrl}${path}`;
    const handlers: WsHandlers = {};

    let ws: WebSocket | undefined;
    let idleTimeoutId: ReturnType<typeof setTimeout> | undefined;
    let cancelled = false;

    // Start the connect deadline immediately so a hung header resolution
    // (e.g. a stuck M2M token fetch) or handshake still times out.
    const connectTimeoutId = setTimeout(() => {
      handlers.error?.(new Error(`WebSocket connection timeout after ${WS_CONNECT_TIMEOUT_MS}ms`));
      ws?.close();
      cancelled = true;
    }, WS_CONNECT_TIMEOUT_MS);

    function resetIdleTimeout() {
      clearTimeout(idleTimeoutId);
      idleTimeoutId = setTimeout(() => {
        handlers.error?.(new Error(`WebSocket idle timeout after ${WS_IDLE_TIMEOUT_MS}ms`));
        ws?.close();
      }, WS_IDLE_TIMEOUT_MS);
    }

    let cleanup = () => {};

    const connect = (resolvedHeaders?: Headers) => {
      // Caller cancelled (or the connect deadline fired) while headers resolved.
      if (cancelled) return;

      const socket = new WebSocket(url, buildWsProtocols(resolvedHeaders));
      ws = socket;

      const onOpen = () => {
        clearTimeout(connectTimeoutId);
        resetIdleTimeout();
        const authFrame = buildAuthFrame(resolvedHeaders);
        if (authFrame) socket.send(authFrame);
        onReady(socket);
      };

      const onMessage = (event: MessageEvent) => {
        resetIdleTimeout();
        try {
          const data = JSON.parse(event.data as string);
          handlers.message?.(data);
        } catch (error) {
          handlers.error?.(error instanceof Error ? error : new Error(String(error)));
        }
      };

      const onError = (event: Event) => {
        const msg = (event as ErrorEvent).message ?? 'unknown WebSocket error';
        handlers.error?.(new Error(`WebSocket error: ${msg}`));
      };

      const onClose = () => {
        clearTimeout(connectTimeoutId);
        clearTimeout(idleTimeoutId);
        cleanup();
        handlers.complete?.();
      };

      socket.addEventListener('open', onOpen);
      socket.addEventListener('message', onMessage);
      socket.addEventListener('error', onError);
      socket.addEventListener('close', onClose);

      cleanup = () => {
        socket.removeEventListener('open', onOpen);
        socket.removeEventListener('message', onMessage);
        socket.removeEventListener('error', onError);
        socket.removeEventListener('close', onClose);
      };
    };

    if (headers instanceof Promise) {
      headers.then(connect).catch((error) => {
        clearTimeout(connectTimeoutId);
        handlers.error?.(error instanceof Error ? error : new Error(String(error)));
      });
    } else {
      connect(headers);
    }

    return {
      handlers,
      socket: () => ws,
      cancel: () => {
        cancelled = true;
        clearTimeout(connectTimeoutId);
        clearTimeout(idleTimeoutId);
        cleanup();
        ws?.close();
      },
    };
  }
}

interface WsHandlers {
  message?: (data: unknown) => void;
  error?: (error: Error) => void;
  complete?: () => void;
}

interface WsConnection {
  handlers: WsHandlers;
  /** The underlying socket, or undefined until headers resolve / connection starts. */
  socket: () => WebSocket | undefined;
  cancel: () => void;
}

/**
 * Build a first-frame auth message for WebSocket connections.
 *
 * Instead of encoding auth in sub-protocols (which appear in HTTP Upgrade
 * headers and get logged by proxies), we send auth as the first JSON frame
 * after the connection is established.
 */
function buildAuthFrame(headers?: Headers): string | undefined {
  const auth = headers?.get('Authorization');
  if (!auth) return undefined;
  return JSON.stringify({ __auth: auth });
}

/**
 * Encode routing headers as WebSocket sub-protocols.
 *
 * Only non-sensitive headers (trace ID) are encoded as sub-protocols.
 * Auth is sent via first-frame message to avoid proxy log exposure.
 */
function buildWsProtocols(headers?: Headers): string[] {
  if (!headers) return [];
  const protocols: string[] = [];

  const traceId = headers.get('X-Trace-Id');
  if (traceId && WS_TOKEN_RE.test(traceId)) {
    protocols.push(`trace.${traceId}`);
  }

  return protocols;
}
