import type { Context, ResolvedMap, SchemaDefinition, TagSelector, Token } from '@putnami/runtime';
import type { HttpRequestContext } from '../../http/http-context.type';
import type { HttpMiddleware } from '../../http/http-middleware.type';
import type { RouteHandler } from '../../http/route.type';
import type { BinaryMeta } from './binary';
import type { EndpointMeta, ResponseDeclarations } from './response-meta';

// ---------------------------------------------------------------------------
// Body content type
// ---------------------------------------------------------------------------

/** Supported content types for request body parsing. */
export type BodyContentType = 'application/json' | 'application/x-www-form-urlencoded';

/** Options for `.body()` schema declaration. */
export interface BodyOptions {
  /** Content type for the request body. Defaults to `'application/json'`. */
  readonly contentType?: BodyContentType;
}

// ---------------------------------------------------------------------------
// EndpointState — single generic carrier for the type-state builder
// ---------------------------------------------------------------------------

export type InjectMap = Record<string, Token | TagSelector>;

/**
 * Compile-time state tracked through fluent calls on `EndpointBuilder`.
 * Each method narrows one slot.
 */
export interface EndpointState {
  params: unknown;
  query: unknown;
  body: unknown;
  headers: unknown;
  returns: unknown;
  bodyStream: boolean;
  returnsStream: boolean;
  /**
   * The endpoint speaks a provider-owned WebSocket wire (a byte stream or a
   * declared subprotocol). Its handler ends the stream by returning, with no
   * terminal value.
   */
  wire: boolean;
  inject: InjectMap | undefined;
}

/** Default state — every slot starts unset. */
export interface EndpointInitialState {
  params: undefined;
  query: undefined;
  body: undefined;
  headers: undefined;
  returns: unknown;
  bodyStream: false;
  returnsStream: false;
  wire: false;
  inject: undefined;
}

/** Update a subset of slots in the state, keeping the rest as-is. */
export type EndpointStateUpdate<S extends EndpointState, U extends Partial<EndpointState>> = {
  [K in keyof EndpointState]: K extends keyof U ? U[K] : S[K];
};

// ---------------------------------------------------------------------------
// EndpointDefinition — the object produced by endpoint()
// ---------------------------------------------------------------------------

export const ENDPOINT_MARKER = 'putnami:endpoint' as const;

export interface EndpointDefinition {
  readonly __endpoint: typeof ENDPOINT_MARKER;
  readonly handler: RouteHandler;
  /** Build-time-only native DI dependencies used by design discovery. */
  readonly dependencies?: readonly string[];
  /** Build-time-only location of the endpoint declaration. */
  readonly provenance?: { path: string; line?: number; symbol?: string };
  readonly schemas?: {
    readonly params?: SchemaDefinition;
    readonly query?: SchemaDefinition;
    readonly body?: SchemaDefinition;
    /** Content type for the request body. Defaults to `'application/json'`. */
    readonly bodyContentType?: BodyContentType;
    /** Raw octet request payload: its media type and its byte bound. */
    readonly bodyBinary?: BinaryMeta;
    readonly headers?: SchemaDefinition;

    readonly returns?: SchemaDefinition;
    /** Raw octet success payload, mirroring `bodyBinary`. */
    readonly returnsBinary?: BinaryMeta;
  };
  readonly responses?: ResponseDeclarations;
  readonly meta?: EndpointMeta;
  readonly middleware?: readonly HttpMiddleware[];
  /** When `true`, the app's default security middleware is skipped for this route (fast lane). */
  readonly minimal?: boolean;
  /**
   * When `true`, CSRF token validation is skipped for the route this endpoint
   * produces. Opt-in per endpoint via `.csrfExempt()` — sugar over the route's
   * `csrfExempt` flag, scoped to this endpoint only.
   */
  readonly csrfExempt?: boolean;
  /**
   * @internal The handler before the generic validation wrapper (but after DI
   * injection). The build-time AOT path rebuilds validation around this, swapping
   * the generic `validateSchema` for compiled validators.
   */
  readonly __rawHandler?: RouteHandler;
}

export function isEndpointDefinition(value: unknown): value is EndpointDefinition {
  return typeof value === 'object' && value !== null && (value as EndpointDefinition).__endpoint === ENDPOINT_MARKER;
}

// ---------------------------------------------------------------------------
// Typed context — extends HttpRequestContext with validated fields and deps
// ---------------------------------------------------------------------------

/**
 * Resolved injected dependencies on the request context.
 *
 * Present only when `.inject()` was called on the builder. The shape is the
 * `ResolvedMap` of the inject token map.
 */
export type EndpointDeps<TInject> = TInject extends InjectMap ? ResolvedMap<TInject> : never;

/**
 * `Omit`, keeping the fields a type declares by name and dropping the index
 * signature it also carries.
 *
 * `HttpRequestContext` intersects `Record<string, unknown>`, so `keyof` it is
 * `string | number` and a plain `Omit` keeps only the index signature: every
 * declared field is erased. A handler then reads `ctx.user` or `ctx.throw` as
 * `unknown` through the index signature instead of its declared type. The key
 * remapping below filters the index signature out; the intersection with
 * `Context` puts it back, so a handler can still carry its own keys.
 */
type OmitDeclared<T, K extends PropertyKey> = {
  [P in keyof T as string extends P ? never : number extends P ? never : P extends K ? never : P]: T[P];
};

/**
 * Request context passed to non-streaming endpoint handlers.
 *
 * Compared to the raw `HttpRequestContext`:
 * - `params` / `queryParams` / `body` are typed by the schemas declared on the builder.
 * - `headerParams()` is added when `.headers()` was called, returning the validated,
 *   typed header values (the raw `headers: Headers` object stays untouched).
 * - `deps` is added when `.inject()` was called, typed by the inject token map.
 */
export type EndpointRequestContext<
  TParams = undefined,
  TQuery = undefined,
  TBody = undefined,
  TInject = undefined,
  THeaders = undefined,
> = Context &
  OmitDeclared<HttpRequestContext, 'params' | 'queryParams' | 'body'> & {
    params: TParams extends undefined ? Record<string, string> | undefined : TParams;
    queryParams: TQuery extends undefined ? () => Record<string, string> : () => TQuery;
    body: TBody extends undefined ? <T>() => Promise<T | undefined> : () => Promise<TBody>;
  } & (TInject extends InjectMap ? { deps: ResolvedMap<TInject> } : Record<never, never>) &
  (THeaders extends undefined ? Record<never, never> : { headerParams: () => THeaders });

// ---------------------------------------------------------------------------
// Stream handler context types — conditional on TBodyStream / TReturnsStream
// ---------------------------------------------------------------------------

export type StreamHandlerContext<
  TParams,
  TQuery,
  TBody,
  TReturns,
  TBodyStream extends boolean,
  TReturnsStream extends boolean,
> = {
  readonly req: Request;
  readonly headers: Headers;
  readonly url: string;
  params: TParams extends undefined ? Record<string, string> | undefined : TParams;
  queryParams: TQuery extends undefined ? () => Record<string, string> : () => TQuery;
  secured: () => boolean;
  host: () => string;
  domain: () => string;
  path: () => string;
  query: () => string;
  /**
   * Aborted when the client disconnects or the stream is otherwise cancelled.
   * The runtime has always passed it; declaring it is what lets a long-running
   * producer stop instead of looping against a caller that is gone.
   */
  readonly signal: AbortSignal;
} & (TBodyStream extends true ? { messages: () => AsyncIterable<TBody> } : Record<string, never>) &
  (TReturnsStream extends true
    ? {
        send: (data: TReturns) => void;
        /**
         * The sequence a continued stream resumes after, when the provider
         * declared `.client({ resume: true })` and the consumer asked to
         * continue. Absent on a fresh stream.
         *
         * A handler that reads it produces only what the consumer has not
         * received; one that ignores it produces the whole stream, and the
         * consumer then sees values it already read.
         */
        readonly resumeFrom?: bigint;
      }
    : Record<string, never>);

export type StreamHandlerReturn<TReturns, TReturnsStream extends boolean> = TReturnsStream extends true
  ? void | Promise<void>
  : TReturns | Promise<TReturns>;
