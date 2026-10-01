import type { InferSchema, ResolvedMap, SchemaDefinition, StreamSchema } from '@putnami/runtime';
import { isStreamSchema, tokenName } from '@putnami/runtime';
import { getExternalCaller, getProjectRoot } from '@putnami/utils';
import type { HttpCacheOptions } from '../../http/cache.middleware';
import { CacheMiddleware } from '../../http/cache.middleware';
import type { CorsOptions } from '../../http/cors.middleware';
import { CorsMiddleware } from '../../http/cors.middleware';
import type { HttpMiddleware } from '../../http/http-middleware.type';
import type { RateLimitOptions } from '../../http/rate-limit.middleware';
import { RateLimitMiddleware } from '../../http/rate-limit.middleware';
import type { RouteHandler, RouteHandlerResult } from '../../http/route.type';
import { SecurityMiddleware } from '../../security/security.middleware';
import type { SecurityGuard, SecurityOptions } from '../../security/security.types';
import type { ClientOperationPolicy } from '../client-contract';
import type {
  BodyContentType,
  BodyOptions,
  EndpointDefinition,
  EndpointInitialState,
  EndpointRequestContext,
  EndpointState,
  EndpointStateUpdate,
  InjectMap,
  StreamHandlerContext,
  StreamHandlerReturn,
} from './endpoint.types';
import { errorCodeToStableCode, IMPLICIT_BAD_REQUEST_CODE, IMPLICIT_INTERNAL_CODE } from './error-codes';
import {
  type BinaryBody,
  type BinaryMeta,
  type BinarySchema,
  type BinaryStreamSchema,
  isBinarySchema,
  validateBinaryMeta,
} from './binary';
import {
  type ByteChunk,
  type ByteStreamSchema,
  isByteStreamSchema,
  isWebSocketSubprotocolToken,
  type ProviderWire,
  RESERVED_SUBPROTOCOL_PREFIX,
} from './byte-stream';
import { buildDefinition, wrapWithInjection } from './endpoint-helpers';

export type { BodyContentType, BodyOptions, EndpointDefinition, EndpointRequestContext } from './endpoint.types';
export { isEndpointDefinition } from './endpoint.types';

import type {
  EndpointMeta,
  ErrorResponseCode,
  ErrorResponseOptions,
  ResponseDeclarations,
  ResponseMeta,
} from './response-meta';
import type { StreamMode } from './stream-endpoint';
import { buildStreamDefinition, type StreamEndpointDefinition } from './stream-endpoint';

// ---------------------------------------------------------------------------
// Type extraction utilities (read slots from the type-state generic)
// ---------------------------------------------------------------------------

/** Extract the params type from an EndpointBuilder */
export type InferParams<T> = T extends EndpointBuilder<infer S> ? S['params'] : never;

/** Extract the query type from an EndpointBuilder */
export type InferQuery<T> = T extends EndpointBuilder<infer S> ? S['query'] : never;

/** Extract the body type from an EndpointBuilder */
export type InferBody<T> = T extends EndpointBuilder<infer S> ? S['body'] : never;

/** Extract the headers type from an EndpointBuilder */
export type InferHeaders<T> = T extends EndpointBuilder<infer S> ? S['headers'] : never;

/** Extract the returns type from an EndpointBuilder */
export type InferReturns<T> = T extends EndpointBuilder<infer S> ? S['returns'] : never;

// ---------------------------------------------------------------------------
// Handler signature derivation — single source of truth
//
// One handler shape regardless of `.inject()`. Resolved deps live on
// `ctx.deps`. Stream modes flip the context shape based on TBodyStream /
// TReturnsStream slots in the state.
// ---------------------------------------------------------------------------

type HandlerForState<S extends EndpointState> = S['wire'] extends true
  ? // A provider-owned wire has no terminal frame: the handler ends the stream
    // by returning, and the socket closes with the code its outcome maps to.
    (ctx: StreamHandlerContext<S['params'], S['query'], S['body'], S['returns'], true, true>) => void | Promise<void>
  : S['bodyStream'] extends true
    ? S['returnsStream'] extends true
      ? // A bidirectional handler streams values with `send` and states its
        // terminal value by returning it: the dispatcher validates that return
        // against the declared `returns` schema and carries it as the `result`
        // payload. A server stream stays `void`, because the published wire
        // forbids a payload on its result.
        (
          ctx: StreamHandlerContext<S['params'], S['query'], S['body'], S['returns'], true, true>,
        ) => StreamHandlerReturn<S['returns'], false>
      : (
          ctx: StreamHandlerContext<S['params'], S['query'], S['body'], S['returns'], true, false>,
        ) => StreamHandlerReturn<S['returns'], false>
    : S['returnsStream'] extends true
      ? (
          ctx: StreamHandlerContext<S['params'], S['query'], S['body'], S['returns'], false, true>,
        ) => void | Promise<void>
      : (
          ctx: EndpointRequestContext<S['params'], S['query'], S['body'], S['inject'], S['headers']>,
        ) => Promise<RouteHandlerResult> | RouteHandlerResult;

type DefinitionForState<S extends EndpointState> = S['bodyStream'] extends true
  ? StreamEndpointDefinition
  : S['returnsStream'] extends true
    ? StreamEndpointDefinition
    : EndpointDefinition;

// ---------------------------------------------------------------------------
// EndpointBuilder — a single generic carries the type-state through the chain.
//
// Each fluent method intersects the state with the slot(s) it narrows, using
// `EndpointStateUpdate`. The runtime cast on the way out is unavoidable in TS
// builder patterns (the class is invariant in its generic) but it is the same
// runtime instance.
// ---------------------------------------------------------------------------

export class EndpointBuilder<S extends EndpointState = EndpointInitialState> {
  private _params?: SchemaDefinition;
  private _query?: SchemaDefinition;
  private _body?: SchemaDefinition;
  private _headers?: SchemaDefinition;
  private _bodyContentType?: BodyContentType;
  private _bodyBinary?: BinaryMeta;
  private _returnsBinary?: BinaryMeta;
  private _returns?: SchemaDefinition;
  private _bodyStream = false;
  private _returnsStream = false;
  private _bodyBytes = false;
  private _returnsBytes = false;
  private _subprotocol?: string;
  private _middleware: HttpMiddleware[] = [];
  private _inject?: InjectMap;
  private _additionalReturns: ResponseMeta[] = [];
  private _errorCodes: ErrorResponseCode[] = [];
  private _errorOptions: Partial<Record<ErrorResponseCode, ErrorResponseOptions>> = {};
  private _errorDetails: Partial<Record<ErrorResponseCode, SchemaDefinition>> = {};
  private _throws: ResponseMeta[] = [];
  private _meta: { -readonly [K in keyof EndpointMeta]?: EndpointMeta[K] } = {};
  private _minimal = false;
  // `undefined` (unset) inherits the api-level default; `true`/`false` is an
  // explicit per-endpoint override. Kept tri-state so `.csrfExempt(false)` can
  // force CSRF enforcement on a route in an otherwise-exempt api().
  private _csrfExempt?: boolean;
  private readonly _provenance = endpointProvenance();

  /**
   * Add a human-readable description to this endpoint.
   * Appears as the `description` field in the OpenAPI operation.
   *
   * @example
   * ```ts
   * endpoint()
   *   .description('List all users with optional pagination')
   *   .query({ page: Optional(Number) })
   *   .handle(...)
   * ```
   */
  description(text: string): this {
    this._meta.description = text;
    return this;
  }

  /** Declare and validate path parameters. */
  params<T extends SchemaDefinition>(schema: T): EndpointBuilder<EndpointStateUpdate<S, { params: InferSchema<T> }>> {
    this._params = schema;
    return this as unknown as EndpointBuilder<EndpointStateUpdate<S, { params: InferSchema<T> }>>;
  }

  /** Declare and validate query string parameters. */
  query<T extends SchemaDefinition>(schema: T): EndpointBuilder<EndpointStateUpdate<S, { query: InferSchema<T> }>> {
    this._query = schema;
    return this as unknown as EndpointBuilder<EndpointStateUpdate<S, { query: InferSchema<T> }>>;
  }

  /**
   * Declare and validate request headers. Values are coerced from strings
   * automatically (like `.params()` and `.query()`).
   *
   * The validated, typed headers are exposed on `ctx.headerParams()`; the raw
   * `ctx.headers` (a `Headers` object) is left untouched. Declared headers are
   * also emitted as OpenAPI `in: "header"` parameters.
   *
   * @example
   * ```ts
   * endpoint()
   *   .headers({ 'x-request-id': Uuid, 'x-tenant': Optional(String) })
   *   .handle((ctx) => {
   *     const { 'x-request-id': requestId } = ctx.headerParams();
   *     return { requestId };
   *   });
   * ```
   */
  headers<T extends SchemaDefinition>(schema: T): EndpointBuilder<EndpointStateUpdate<S, { headers: InferSchema<T> }>> {
    this._headers = schema;
    return this as unknown as EndpointBuilder<EndpointStateUpdate<S, { headers: InferSchema<T> }>>;
  }

  /**
   * Declare and validate request body, or `Stream(schema)` for a stream of incoming messages.
   *
   * By default the body is parsed as `application/json`. Pass
   * `{ contentType: 'application/x-www-form-urlencoded' }` to parse form-encoded bodies — values
   * are coerced from strings automatically (like `.params()` and `.query()`).
   *
   * @example JSON body (default)
   * ```ts
   * endpoint().body({ name: String, email: Email }).handle(...)
   * ```
   *
   * @example Form-encoded body (OAuth2 token endpoint, etc.)
   * ```ts
   * endpoint()
   *   .body({ grant_type: String, code: Optional(String) }, { contentType: 'application/x-www-form-urlencoded' })
   *   .handle(async (ctx) => {
   *     const body = await ctx.body();
   *   });
   * ```
   */
  body<T extends SchemaDefinition>(
    schema: StreamSchema<T>,
  ): EndpointBuilder<EndpointStateUpdate<S, { body: InferSchema<T>; bodyStream: true }>>;
  body(
    schema: BinaryStreamSchema,
  ): EndpointBuilder<EndpointStateUpdate<S, { body: ReadableStream<Uint8Array>; bodyStream: false }>>;
  body(schema: BinarySchema): EndpointBuilder<EndpointStateUpdate<S, { body: BinaryBody; bodyStream: false }>>;
  body(
    schema: ByteStreamSchema,
  ): EndpointBuilder<EndpointStateUpdate<S, { body: ByteChunk; bodyStream: true; wire: true }>>;
  body<T extends SchemaDefinition>(
    schema: T,
    options?: BodyOptions,
  ): EndpointBuilder<EndpointStateUpdate<S, { body: InferSchema<T>; bodyStream: false }>>;
  body(
    schema: SchemaDefinition | StreamSchema | BinarySchema | ByteStreamSchema,
    options?: BodyOptions,
    // biome-ignore lint/suspicious/noExplicitAny: implementation signature widens overloads
  ): EndpointBuilder<any> {
    this._bodyBytes = false;
    if (isByteStreamSchema(schema)) {
      // Octets have no member schema: the declaration is the whole message.
      this._body = undefined;
      this._bodyStream = true;
      this._bodyBytes = true;
      this._bodyContentType = undefined;
    } else if (isStreamSchema(schema)) {
      this._body = schema.schema;
      this._bodyStream = true;
    } else if (isBinarySchema(schema)) {
      validateBinaryMeta(schema);
      // The declaration carries the whole body: there is no member schema to
      // validate and no content type to negotiate past the declared one.
      this._body = undefined;
      this._bodyStream = false;
      this._bodyContentType = undefined;
      this._bodyBinary = {
        mediaType: schema.mediaType,
        maxBytes: schema.maxBytes,
        ...(schema.streamed ? { streamed: true } : {}),
      };
    } else {
      this._body = schema;
      this._bodyStream = false;
      this._bodyContentType = options?.contentType;
    }
    return this;
  }

  /**
   * Declare the primary success response shape, or `Stream(schema)` for a stream of outgoing
   * messages.
   *
   * The schema feeds dev-mode response validation and the OpenAPI `200` response. For additional
   * status codes, use `.response(status, ...)`.
   *
   * @example
   * ```ts
   * endpoint().returns({ id: String, name: String }).handle(...)
   * ```
   *
   * @example Streaming response
   * ```ts
   * endpoint().returns(Stream({ event: String })).handle(async (ctx) => { ctx.send({ event: 'hi' }); })
   * ```
   */
  returns<T extends SchemaDefinition>(
    schema: StreamSchema<T>,
  ): EndpointBuilder<EndpointStateUpdate<S, { returns: InferSchema<T>; returnsStream: true }>>;
  returns(
    schema: BinaryStreamSchema,
  ): EndpointBuilder<EndpointStateUpdate<S, { returns: ReadableStream<Uint8Array>; returnsStream: false }>>;
  returns(schema: BinarySchema): EndpointBuilder<EndpointStateUpdate<S, { returns: BinaryBody; returnsStream: false }>>;
  returns(
    schema: ByteStreamSchema,
  ): EndpointBuilder<EndpointStateUpdate<S, { returns: ByteChunk; returnsStream: true; wire: true }>>;
  returns<T extends SchemaDefinition>(
    schema: T,
  ): EndpointBuilder<EndpointStateUpdate<S, { returns: InferSchema<T>; returnsStream: false }>>;
  // biome-ignore lint/suspicious/noExplicitAny: implementation signature widens overloads
  returns(schema: SchemaDefinition | StreamSchema | BinarySchema | ByteStreamSchema): EndpointBuilder<any> {
    this._returnsBytes = false;
    if (isByteStreamSchema(schema)) {
      this._returns = undefined;
      this._returnsStream = true;
      this._returnsBytes = true;
    } else if (isStreamSchema(schema)) {
      this._returns = schema.schema;
      this._returnsStream = true;
    } else if (isBinarySchema(schema)) {
      validateBinaryMeta(schema);
      this._returns = undefined;
      this._returnsStream = false;
      this._returnsBinary = {
        mediaType: schema.mediaType,
        maxBytes: schema.maxBytes,
        ...(schema.streamed ? { streamed: true } : {}),
      };
    } else {
      this._returns = schema;
      this._returnsStream = false;
    }
    return this;
  }

  /**
   * Declare an additional success response for a specific status code.
   *
   * `.returns()` covers the primary success type. Use `.response()` to document additional
   * status codes (e.g. `201 Created` alongside `200 OK`).
   *
   * @example
   * ```ts
   * endpoint()
   *   .returns({ id: String, name: String })             // 200 default
   *   .response(201, 'Created', { id: String })          // additional status
   *   .response(204, 'No content')                       // additional status, no body
   *   .handle(...)
   * ```
   */
  response(status: number, schema: SchemaDefinition): this;
  response(status: number, description: string, schema?: SchemaDefinition): this;
  response(status: number, description: string): this;
  response(status: number, descriptionOrSchema?: string | SchemaDefinition, maybeSchema?: SchemaDefinition): this {
    let description: string | undefined;
    let schema: SchemaDefinition | undefined;
    if (typeof descriptionOrSchema === 'string') {
      description = descriptionOrSchema;
      schema = maybeSchema;
    } else {
      schema = descriptionOrSchema as SchemaDefinition | undefined;
    }
    this._additionalReturns.push({ status, description, schema });
    return this;
  }

  /**
   * Declare framework-known error codes this endpoint may emit.
   *
   * The OpenAPI generator maps each code to the standard error response envelope
   * and HTTP status. Use `.mayThrowDetails()` when the error carries a typed `details`
   * body, and `.throws()` for custom descriptions.
   *
   * @example
   * ```ts
   * endpoint().mayThrow('NotFound', 'Conflict').handle(...)
   * ```
   */
  mayThrow(...codes: ErrorResponseCode[]): this {
    this._errorCodes.push(...codes);
    return this;
  }

  /** Declare one stable error code and whether safe generated calls may retry it. */
  mayThrowWith(code: ErrorResponseCode, options: ErrorResponseOptions): this {
    this._errorCodes.push(code);
    this._errorOptions[code] = { retryable: options.retryable };
    return this;
  }

  /**
   * Declare one stable error code and the schema of the `details` member its error
   * envelope carries.
   *
   * The first-party contract publishes the schema as that code's error schema, and
   * generated clients validate `details` against it and expose it typed on the error
   * for that code. It describes `details` only, never the `{code, error, message}`
   * envelope, and declares no retry classification: chain `.mayThrowWith()` for one.
   * The handler throws the matching exception with the details as its response
   * object. A later declaration for the same code replaces the earlier one.
   *
   * It throws for `'BadRequest'` and `'InternalServerError'`: every endpoint answers
   * those implicit codes (`http.bad_request`, `http.internal_server`), and the
   * framework owns their response, so a declared schema would describe bodies the
   * endpoint never writes. Go's `MayThrowDetails` refuses the same two codes.
   *
   * @example
   * ```ts
   * endpoint()
   *   .mayThrowDetails('Conflict', { rejections: ArrayOf({ project: String, error: String }) })
   *   .handle(() => {
   *     throw new ConflictException({ rejections: [{ project: 'a', error: 'image not found' }] });
   *   });
   * ```
   */
  mayThrowDetails(code: ErrorResponseCode, schema: SchemaDefinition): this {
    const stable = errorCodeToStableCode[code];
    if (stable === IMPLICIT_BAD_REQUEST_CODE || stable === IMPLICIT_INTERNAL_CODE) {
      throw new Error(
        `endpoint.mayThrowDetails: ${JSON.stringify(code)} (${stable}) is a framework-owned implicit error whose response the framework writes (request validation answers it); declare a code of your own`,
      );
    }
    this._errorCodes.push(code);
    this._errorDetails[code] = schema;
    return this;
  }

  /**
   * Document possible error responses for this endpoint.
   *
   * Two call shapes:
   * - **Inline**: `throws(status, description, schema?)` — declare a single error inline.
   * - **Spread**: `throws(...specs)` — pass one or more `ResponseMeta` objects, useful for
   *   spreading a reusable bundle.
   *
   * @example Inline
   * ```ts
   * endpoint().throws(404, 'Not found').handle(...)
   * ```
   *
   * @example Reusable bundle
   * ```ts
   * const AuthErrors: ResponseMeta[] = [
   *   { status: 401, description: 'Unauthorized' },
   *   { status: 403, description: 'Forbidden' },
   * ];
   * endpoint().throws(...AuthErrors).throws(404, 'Not found').handle(...)
   * ```
   */
  throws(status: number, description: string, schema?: SchemaDefinition): this;
  throws(...specs: ResponseMeta[]): this;
  throws(
    statusOrFirstSpec: number | ResponseMeta,
    descriptionOrNextSpec?: string | ResponseMeta,
    schemaOrNextSpec?: SchemaDefinition | ResponseMeta,
    ...rest: ResponseMeta[]
  ): this {
    if (typeof statusOrFirstSpec === 'number') {
      this._throws.push({
        status: statusOrFirstSpec,
        description: descriptionOrNextSpec as string,
        schema: schemaOrNextSpec as SchemaDefinition | undefined,
      });
      return this;
    }
    const args = [statusOrFirstSpec, descriptionOrNextSpec, schemaOrNextSpec, ...rest].filter(
      (a): a is ResponseMeta => a !== undefined,
    );
    this._throws.push(...args);
    return this;
  }

  /**
   * Declare dependencies to resolve from the current DI scope.
   *
   * Resolved values are exposed on `ctx.deps`, typed by the inject token map.
   *
   * @example
   * ```ts
   * endpoint()
   *   .params({ id: Uuid })
   *   .inject({ userService: UserService, db: Database })
   *   .handle(async (ctx) => ctx.deps.userService.getUser(ctx.params.id));
   * ```
   */
  inject<M extends InjectMap>(tokens: M): EndpointBuilder<EndpointStateUpdate<S, { inject: M }>> {
    this._inject = tokens;
    return this as unknown as EndpointBuilder<EndpointStateUpdate<S, { inject: M }>>;
  }

  /** Apply HTTP cache headers (Cache-Control, ETag) to this endpoint. */
  cache(options: HttpCacheOptions = {}): this {
    this._middleware.push(CacheMiddleware(options));
    this._meta.cache = options;
    return this;
  }

  /** Enable CORS for this endpoint with the given options. */
  cors(options: CorsOptions = {}): this {
    this._middleware.push(CorsMiddleware(options));
    this._meta.cors = options;
    return this;
  }

  /** Apply rate limiting to this endpoint. */
  rateLimit(options: RateLimitOptions = {}): this {
    this._middleware.push(RateLimitMiddleware(options));
    this._meta.rateLimit = { windowMs: options.windowMs, max: options.max };
    return this;
  }

  /**
   * Require authentication and enforce access rules for this endpoint.
   *
   * @example Require any authenticated user
   * ```ts
   * endpoint().secure().handle(...)
   * ```
   *
   * @example Require specific roles and scopes
   * ```ts
   * endpoint().secure({ roles: ['admin'], scopes: ['write'] }).handle(...)
   * ```
   *
   * @example Custom guard function
   * ```ts
   * endpoint().secure((user, ctx) => user.orgId === ctx.params?.orgId).handle(...)
   * ```
   */
  secure(optionsOrGuard?: SecurityOptions | SecurityGuard): this {
    this._middleware.push(SecurityMiddleware(optionsOrGuard ?? {}));
    if (typeof optionsOrGuard === 'function') {
      this._meta.security = {};
      this._meta.customSecurityGuard = true;
    } else {
      this._meta.security = optionsOrGuard ?? {};
      this._meta.customSecurityGuard = false;
    }
    return this;
  }

  /**
   * Declare the WebSocket subprotocol this endpoint's provider-owned wire
   * negotiates, for example `'putnami.events.v1'`.
   *
   * On a bidirectional typed stream — `.body(Stream(In))` and
   * `.returns(Stream(Out))` — it replaces the first-party conversation: each
   * message is one JSON value of `In` or `Out`, with no envelope, and the
   * provider's own protocol owns everything after the upgrade. On a byte
   * stream it names the token the tunnel negotiates. The token must be a valid
   * RFC 9110 token outside the first-party `putnami.service.` namespace.
   *
   * @example
   * ```ts
   * endpoint()
   *   .body(Stream(EventClientFrame))
   *   .returns(Stream(EventServerFrame))
   *   .subprotocol('putnami.events.v1')
   *   .handle(async (ctx) => {
   *     for await (const frame of ctx.messages()) ctx.send(answer(frame));
   *   });
   * ```
   */
  subprotocol(token: string): EndpointBuilder<EndpointStateUpdate<S, { wire: true }>> {
    this._subprotocol = token;
    return this as unknown as EndpointBuilder<EndpointStateUpdate<S, { wire: true }>>;
  }

  /**
   * Declare generated-client security, idempotency, and resilience semantics.
   * Stream mode, transports, and declared errors are derived from the endpoint
   * definition so they cannot drift from what the server actually exposes.
   */
  client(policy: ClientOperationPolicy): this {
    this._meta.client = policy;
    return this;
  }

  /**
   * Skip the app's always-on security middleware (origin guard, security
   * headers) for this route — the per-route counterpart of `http({ secure: false })`.
   *
   * A fast lane for trusted, unauthenticated hot paths. Does **not** affect
   * `.secure()` (auth) or any middleware you add explicitly on the endpoint.
   *
   * @example
   * ```ts
   * endpoint().minimal().handle(() => ({ status: 'ok' }));
   * ```
   */
  minimal(): this {
    this._minimal = true;
    return this;
  }

  /**
   * Skip CSRF token validation for the route this endpoint produces.
   *
   * Opt-in per endpoint. Sets the route's existing `csrfExempt` flag for **this
   * endpoint only** — it never widens exemption to sibling routes. Use for
   * trusted machine-to-machine or webhook endpoints that authenticate with a
   * bearer token or signature rather than a browser session cookie.
   *
   * Pass `false` to explicitly **force CSRF enforcement** on this route even
   * when the enclosing `api()` is CSRF-exempt by default; omitting the call
   * leaves the route on the api-level default.
   *
   * @example
   * ```ts
   * endpoint().csrfExempt().body({ event: String }).handle(...)
   * ```
   */
  csrfExempt(exempt = true): this {
    this._csrfExempt = exempt;
    return this;
  }

  /**
   * The provider-owned wire this endpoint declares, checked against every rule
   * the client contract enforces, so an authoring mistake stops at registration
   * instead of reaching a document no reader accepts.
   */
  private providerWire(): ProviderWire | undefined {
    const bytes = this._bodyBytes || this._returnsBytes;
    if (!bytes && this._subprotocol === undefined) return undefined;
    if (bytes && !(this._bodyBytes && this._returnsBytes)) {
      throw new Error(
        'endpoint: a byte stream carries octets both ways; declare .body(ByteStream()) and .returns(ByteStream())',
      );
    }
    if (!this._bodyStream || !this._returnsStream) {
      throw new Error(
        'endpoint: .subprotocol() declares a provider-owned wire, which carries a bidirectional stream; declare .body() and .returns() as streams',
      );
    }
    const subprotocol = this._subprotocol;
    if (subprotocol !== undefined) {
      if (!isWebSocketSubprotocolToken(subprotocol)) {
        throw new Error(`endpoint: subprotocol ${JSON.stringify(subprotocol)} is not a negotiable websocket token`);
      }
      if (subprotocol.startsWith(RESERVED_SUBPROTOCOL_PREFIX)) {
        throw new Error(
          `endpoint: subprotocol ${JSON.stringify(subprotocol)} is in the first-party namespace ${JSON.stringify(RESERVED_SUBPROTOCOL_PREFIX)}`,
        );
      }
    }
    if (this._meta.client?.resume) {
      throw new Error(
        "endpoint: a provider-owned wire resumes by its own protocol; .client({ resume }) is the first-party conversation's",
      );
    }
    if (this._meta.client?.sseContinuation !== undefined) {
      throw new Error(
        "endpoint: a provider-owned wire is a WebSocket and carries no SSE; .client({ sseContinuation }) is the first-party SSE transport's",
      );
    }
    return { bytes, ...(subprotocol !== undefined ? { subprotocol } : {}) };
  }

  /**
   * Provide the handler function — finalises the endpoint definition.
   *
   * The handler always receives a single `ctx` argument. When `.inject()` was called, resolved
   * dependencies are available on `ctx.deps`.
   *
   * When `.body()` or `.returns()` use `Stream()`, produces a `StreamEndpointDefinition`.
   * Otherwise produces a standard `EndpointDefinition`.
   */
  handle(handler: HandlerForState<S>): DefinitionForState<S> {
    const wire = this.providerWire();
    if (this._bodyStream || this._returnsStream) {
      let mode: StreamMode;
      if (this._bodyStream && this._returnsStream) {
        mode = 'bidirectional';
      } else if (this._returnsStream) {
        mode = 'server';
      } else {
        mode = 'client';
      }

      const hasStreamErrorCodes = this._errorCodes.length > 0;
      const hasStreamThrows = this._throws.length > 0;
      const streamResponses: ResponseDeclarations | undefined =
        hasStreamErrorCodes || hasStreamThrows
          ? {
              errorCodes: hasStreamErrorCodes ? this._errorCodes : undefined,
              errorOptions: hasStreamErrorCodes ? this._errorOptions : undefined,
              errorDetails: hasStreamErrorCodes ? this._errorDetails : undefined,
              throws: hasStreamThrows ? this._throws : undefined,
            }
          : undefined;
      const streamMeta = Object.keys(this._meta).length > 0 ? this._meta : undefined;
      const streamHandler = this._inject
        ? wrapWithInjection(handler as unknown as Parameters<typeof wrapWithInjection>[0], this._inject)
        : (handler as unknown as Parameters<typeof buildStreamDefinition>[1]);
      return buildStreamDefinition(
        mode,
        // Handler signature is user-generic; stream definition expects a mode-specific context.
        streamHandler as unknown as Parameters<typeof buildStreamDefinition>[1],
        {
          params: this._params,
          query: this._query,
          body: this._body,
          returns: this._returns,
        },
        streamResponses,
        streamMeta as EndpointMeta | undefined,
        this._middleware,
        wire,
      ) as DefinitionForState<S>;
    }

    const finalHandler: RouteHandler = this._inject
      ? wrapWithInjection(handler as unknown as Parameters<typeof wrapWithInjection>[0], this._inject)
      : (handler as unknown as RouteHandler);

    const hasAdditionalReturns = this._additionalReturns.length > 0;
    const hasErrorCodes = this._errorCodes.length > 0;
    const hasThrows = this._throws.length > 0;
    const responses: ResponseDeclarations | undefined =
      hasAdditionalReturns || hasErrorCodes || hasThrows
        ? {
            returns: hasAdditionalReturns ? this._additionalReturns : undefined,
            errorCodes: hasErrorCodes ? this._errorCodes : undefined,
            errorOptions: hasErrorCodes ? this._errorOptions : undefined,
            errorDetails: hasErrorCodes ? this._errorDetails : undefined,
            throws: hasThrows ? this._throws : undefined,
          }
        : undefined;

    const meta = Object.keys(this._meta).length > 0 ? (this._meta as EndpointMeta) : undefined;

    return buildDefinition(
      finalHandler,
      {
        params: this._params,
        query: this._query,
        body: this._body,
        bodyContentType: this._bodyContentType,
        bodyBinary: this._bodyBinary,
        headers: this._headers,
        returns: this._returns,
        returnsBinary: this._returnsBinary,
      },
      this._middleware,
      responses,
      meta,
      this._minimal,
      this._csrfExempt,
      this._inject ? Object.values(this._inject).map(tokenName).sort() : undefined,
      this._provenance,
    ) as DefinitionForState<S>;
  }
}

// Re-export so call sites can pull the resolved deps map type.
export type { ResolvedMap };

// ---------------------------------------------------------------------------
// endpoint() — two-mode entry point
// ---------------------------------------------------------------------------

/**
 * Define an endpoint handler.
 *
 * **Simple mode** — pass a handler directly, zero ceremony:
 * ```ts
 * export default endpoint((ctx) => {
 *   return { hello: 'world' };
 * });
 * ```
 *
 * **Builder mode** — chain validation, docs, and more:
 * ```ts
 * export default endpoint()
 *   .params({ id: Uuid })
 *   .body({ name: String, email: Email })
 *   .cors({ origin: 'https://example.com' })
 *   .rateLimit({ max: 50 })
 *   .handle(async (ctx) => {
 *     return { id: ctx.params.id, name: ctx.body.name };
 *   });
 * ```
 *
 * **Stream mode** — use `Stream()` in `.body()` or `.returns()`:
 * ```ts
 * export default endpoint()
 *   .body(Stream({ type: String, data: String }))
 *   .returns(Stream({ event: String, payload: String }))
 *   .handle(async (ctx) => {
 *     ctx.send({ event: 'welcome', payload: 'hello' });
 *     for await (const msg of ctx.messages()) {
 *       ctx.send({ event: 'echo', payload: msg.data });
 *     }
 *   });
 * ```
 */
export function endpoint(handler: RouteHandler): EndpointDefinition;
export function endpoint(): EndpointBuilder;
export function endpoint(handler?: RouteHandler): EndpointDefinition | EndpointBuilder {
  if (handler) {
    return buildDefinition(
      handler,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      endpointProvenance(),
    );
  }
  return new EndpointBuilder();
}

function endpointProvenance(): EndpointDefinition['provenance'] {
  const caller = getExternalCaller(getProjectRoot());
  if (!caller) return undefined;
  return {
    path: caller.filePath,
    line: caller.lineNumber,
    ...(caller.functionName ? { symbol: caller.functionName } : {}),
  };
}
