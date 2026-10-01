import type { SchemaDefinition } from '@putnami/runtime';
import type { HttpMiddleware } from '../../http/http-middleware.type';
import type { ProviderWire } from './byte-stream';
import type { EndpointMeta, ResponseDeclarations } from './response-meta';

// ---------------------------------------------------------------------------
// StreamEndpointDefinition — produced by endpoint().handle() when Stream() is used
// ---------------------------------------------------------------------------

const STREAM_ENDPOINT_MARKER = 'putnami:stream-endpoint' as const;

export type StreamMode = 'server' | 'client' | 'bidirectional';

export interface StreamEndpointDefinition {
  readonly __streamEndpoint: typeof STREAM_ENDPOINT_MARKER;
  readonly mode: StreamMode;
  // biome-ignore lint/suspicious/noExplicitAny: handler context varies by mode
  readonly handler: (ctx: any) => Promise<unknown>;
  readonly schemas?: {
    readonly params?: SchemaDefinition;
    readonly query?: SchemaDefinition;
    readonly body?: SchemaDefinition;
    readonly returns?: SchemaDefinition;
  };
  readonly responses?: ResponseDeclarations;
  readonly meta?: EndpointMeta;
  readonly middleware?: readonly HttpMiddleware[];
  /**
   * A provider-owned WebSocket wire: raw octets, or JSON values of the
   * declared messages under the provider's own subprotocol. The upgrade is its
   * admission and there is no first-party conversation. Absent on a
   * first-party stream.
   */
  readonly wire?: ProviderWire;
}

export function isStreamEndpointDefinition(value: unknown): value is StreamEndpointDefinition {
  return (
    typeof value === 'object' &&
    value !== null &&
    (value as StreamEndpointDefinition).__streamEndpoint === STREAM_ENDPOINT_MARKER
  );
}

// ---------------------------------------------------------------------------
// StreamHandlerContext — the context passed to stream handlers
// ---------------------------------------------------------------------------

/** Base context shared across all stream modes */
export interface StreamHandlerBaseContext {
  readonly req: Request;
  readonly headers: Headers;
  readonly url: string;
  params: Record<string, string> | undefined;
  queryParams: () => Record<string, string>;
  secured: () => boolean;
  host: () => string;
  domain: () => string;
  path: () => string;
  query: () => string;
  /**
   * Aborted when the client disconnects (or the stream is otherwise cancelled).
   * Long-running server-stream loops should check `signal.aborted` / await on it
   * so they stop producing instead of running forever against a gone client.
   */
  readonly signal: AbortSignal;
}

/** Context for server-stream: only ctx.send() available */
export interface ServerStreamContext<TReturns = unknown> extends StreamHandlerBaseContext {
  send: (data: TReturns) => void;
  /**
   * The sequence a continued stream resumes after, when the provider declared
   * `.client({ resume: true })` and the consumer asked to continue.
   *
   * It is absent on a fresh stream. A handler that reads it produces only what
   * the consumer has not received; one that ignores it produces the whole
   * stream, and the consumer then sees values it already read.
   */
  readonly resumeFrom?: bigint;
}

/** Context for client-stream: only ctx.messages() available */
export interface ClientStreamContext<TBody = unknown> extends StreamHandlerBaseContext {
  messages: () => AsyncIterable<TBody>;
}

// ---------------------------------------------------------------------------
// buildStreamDefinition
// ---------------------------------------------------------------------------

export function buildStreamDefinition(
  mode: StreamMode,
  // biome-ignore lint/suspicious/noExplicitAny: handler context varies by mode
  handler: (ctx: any) => Promise<unknown>,
  schemas?: StreamEndpointDefinition['schemas'],
  responses?: ResponseDeclarations,
  meta?: EndpointMeta,
  middleware?: readonly HttpMiddleware[],
  wire?: ProviderWire,
): StreamEndpointDefinition {
  return {
    __streamEndpoint: STREAM_ENDPOINT_MARKER,
    mode,
    handler,
    schemas,
    ...(responses ? { responses } : {}),
    ...(meta ? { meta } : {}),
    ...(middleware?.length ? { middleware } : {}),
    ...(wire ? { wire } : {}),
  };
}
