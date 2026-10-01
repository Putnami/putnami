import { StreamRelay } from './connect-stream';
import type {
  ClientContractOperation,
  ClientProtobufDescriptor,
  ClientResiliencePolicy,
  ClientSchema,
  ClientTransportContract,
} from '@putnami/application';
import type { BinaryPayload, StreamedBinaryPayload } from './binary';
import { CircuitBreaker } from './circuit-breaker';
import { CredentialRegistryClosedError, rejectRequestCredentials } from './credential';
import { ServiceWebSocketTransport } from './service-ws-transport';
import { type ByteStream, type FrameStream, ProviderWebSocketTransport } from './provider-ws-transport';
import { startServiceStreamCall } from './service-telemetry';
import { StreamSession, type StreamBudgets } from './stream-session';
import { assertSuccessBodyDeliverable, type SuccessBody } from './success-body';
import { type ConnectEncoding, ConnectTransport, type ProtoMeta, protoMetaFromDescriptor } from './connect-transport';
import {
  ClientCanceledError,
  ClientDeadlineError,
  ClientError,
  ClientFrameworkError,
  ClientResponseContractError,
  ClientRequestEncodingError,
  ClientServiceConfigError,
  ClientRetryExhaustedError,
  ClientTimeoutError,
  ClientTransportUnavailableError,
} from './errors';
import { HttpTransport } from './http-transport';
import { type GeneratedClientDesign, resolveClientFeatureTrace } from './generated-client-design';
import { type RetryConfig, retryInterceptor } from './retry';
import {
  invalidationTag,
  isResponseCacheInterceptor,
  type ResponseCacheInterceptor,
  type ResponseFieldValue,
} from './response-cache';
import { resolveClientResilience } from './service-resilience';
import { SseTransport } from './sse-transport';
import type { DuplexStream, StreamObserver } from './stream.type';
import {
  type ClientRequest,
  type ClientResponse,
  type DisposableInterceptor,
  type Interceptor,
  isDisposableInterceptor,
  type Transport,
} from './transport.type';
import { assertHttpUrl } from './url';
import { WebSocketTransport } from './ws-transport';

/**
 * Transport mode determined at generation time.
 */
export type TransportMode = 'http' | 'connect';

/**
 * Configuration for a generated client.
 */
export interface ClientConfig {
  /** Validated by the safe binding; maps fixed unary REST operations to owner paths. */
  operationPaths?: Readonly<Record<string, string>>;
  /** @internal Registered binding policy and immutable endpoint client factory. */
  endpointBinding?: (endpoint: string) => { url: string; create: () => BaseClient };
  /** Base URL of the target service */
  baseUrl: string;
  /** Transport mode: 'http' for REST/JSON, 'connect' for Connect protocol */
  transport: TransportMode;
  /** Proto package name (required for Connect transport) */
  packageName?: string;
  /**
   * Connect encoding mode: 'json' (default) or 'proto' (binary).
   * Generated clients set this automatically from embedded proto metadata.
   */
  encoding?: ConnectEncoding;
  /**
   * Proto message metadata for binary encoding/decoding.
   * Generated clients embed this automatically — no manual config needed.
   */
  protoMeta?: ProtoMeta;
  /**
   * Client identity — declares which service is making the call.
   * Sent as `X-Client-Id` header on every request.
   * The receiving service can use `requireClient(['orders-service'])` to restrict access.
   */
  clientId?: string;
  /**
   * Per-attempt request timeout in ms. Default: 30000.
   *
   * This bounds each individual attempt. It is also used as the overall
   * deadline for the whole retry sequence (attempts + backoff), so retries
   * can never amplify worst-case latency beyond a single `timeoutMs`: the
   * per-attempt timeout is clamped to the remaining budget and a request can
   * fail with `ClientRetryExhaustedError` once the deadline is reached.
   */
  timeoutMs?: number;
  /**
   * Maximum response body size in bytes. Responses larger than this fail with
   * `ClientResponseSizeError` instead of buffering unbounded. `0`/unset selects
   * the 32 MiB default (Go parity — mirrors the Go client's `MaxResponseSize`).
   */
  maxResponseSize?: number;
  /**
   * Gzip request bodies on the Connect transport (`grpc-encoding: gzip`).
   * Default: `false` — responses are always decoded when the server compresses
   * them, but compressing small RPC bodies usually costs more than it saves.
   */
  compressRequests?: boolean;
  /** Retry configuration */
  retry?: Partial<RetryConfig>;
  /** Additional interceptors applied in order (before built-in retry) */
  interceptors?: Interceptor[];
  /** @internal Interceptors rerun inside every retry attempt (for credential refresh). */
  attemptInterceptors?: Interceptor[];
  /** @internal Interceptors that prepare authenticated streaming handshakes. */
  streamInterceptors?: Interceptor[];
  /** @internal Exact producer identity embedded by the client generator. */
  design?: GeneratedClientDesign;
  /** @internal Generated first-party service identity for transport errors. */
  serviceId?: string;
  /**
   * @internal The consuming binding opted in to carrying the provider's
   * free-text error `message` onto `ClientFrameworkError`
   * (`ServiceBinding.carryRemoteMessage`). Unset keeps the local synthetic
   * message.
   */
  carryRemoteMessage?: boolean;
  /** @internal Immutable operation policies embedded by the generator. */
  operationContracts?: Readonly<Record<string, ClientContractOperation>>;
  /** @internal Provider document resilience defaults. */
  clientDefaults?: ClientResiliencePolicy;
  /** @internal Neutral component schemas for declared response/error refs. */
  clientSchemas?: Readonly<Record<string, ClientSchema>>;
  /**
   * @internal Provider-published protobuf descriptor, embedded by the generator
   * when the contract declares a Connect transport. It is what lets a first-party
   * call ride Connect without the consumer configuring a codec.
   */
  clientProtobuf?: ClientProtobufDescriptor;
}

/**
 * Abstract base class for all generated clients.
 *
 * Provides the request pipeline: interceptors → retry → transport.
 * Generated clients extend this and expose typed methods that call `this.request()`.
 *
 * For streaming RPCs, use `this.stream()` (server-streaming — Connect first on
 * a Connect client, WebSocket fallback) or `this.streamDuplex()`
 * (client/bidi-streaming — WebSocket, which Connect over HTTP/1.1 cannot carry).
 */
export abstract class BaseClient {
  private readonly endpointBinding?: ClientConfig['endpointBinding'];
  private readonly endpointClients = new Map<string, BaseClient>();
  private readonly baseUrl: string;
  /** Service name for telemetry and error reporting */
  abstract readonly serviceName: string;

  private readonly transport: Transport;
  /**
   * The Connect transport when this client speaks Connect, so server-streaming
   * RPCs can ride Connect envelope frames before falling back to WebSocket.
   */
  private readonly connectTransport?: ConnectTransport;
  /**
   * One Connect transport per declared codec, built from the provider's own
   * descriptor. A first-party call is routed to whichever of these the
   * operation declared first; there is no consumer-side codec choice.
   */
  private readonly firstPartyConnect = new Map<ConnectEncoding, ConnectTransport>();
  private readonly sseTransport: SseTransport;
  private readonly wsTransport: WebSocketTransport;
  private readonly serviceWsTransport: ServiceWebSocketTransport;
  private readonly providerWsTransport: ProviderWebSocketTransport;
  /**
   * One circuit per first-party operation, shared by every stream of that
   * operation. Streams never reach the unary `serviceCircuitInterceptor`: the
   * conversation owns the socket for its whole life and never traverses the
   * chain, so the breaker lives here and every write goes through the session.
   */
  private readonly streamBreakers = new Map<string, CircuitBreaker>();
  private readonly chain: (req: ClientRequest) => Promise<ClientResponse>;
  private readonly headerChain: (req: ClientRequest) => Promise<ClientResponse>;
  private readonly timeoutMs: number;
  private readonly design?: GeneratedClientDesign;
  private readonly operationPaths?: Readonly<Record<string, string>>;
  private readonly operationContracts?: Readonly<Record<string, ClientContractOperation>>;
  private readonly clientDefaults?: ClientResiliencePolicy;
  private readonly clientSchemas?: Readonly<Record<string, ClientSchema>>;
  /**
   * The consuming binding's opt-in to provider error prose. It is stamped on
   * every request this client builds, so each decode site reads it exactly
   * where it already reads the call's own secrets.
   */
  private readonly carryRemoteMessage: boolean;
  private readonly activeServiceStreams = new Set<{ cancel(): void }>();
  private disposed = false;
  /**
   * Interceptors that own background resources (e.g. the circuit breaker's
   * health-probe timer). Torn down by {@link dispose}.
   */
  private readonly disposables: DisposableInterceptor[];
  /** Response cache interceptors whose entries {@link invalidateResponses} drops. */
  private readonly invalidators: ResponseCacheInterceptor[];

  constructor(config: ClientConfig) {
    const baseUrl = assertHttpUrl(config.baseUrl, 'ClientConfig: baseUrl');
    this.baseUrl = baseUrl;
    this.endpointBinding = config.endpointBinding;
    this.timeoutMs = resolveTimeoutMs(config.timeoutMs);
    this.design = config.design;
    this.operationContracts = config.operationContracts;
    this.operationPaths = config.operationPaths ? Object.freeze({ ...config.operationPaths }) : undefined;
    this.clientDefaults = config.clientDefaults;
    this.clientSchemas = config.clientSchemas;
    this.carryRemoteMessage = config.carryRemoteMessage === true;

    if (config.transport === 'connect') {
      const packageName = config.packageName;
      if (!packageName) throw new Error('ClientConfig: packageName is required for Connect transport');
      this.connectTransport = new ConnectTransport(
        baseUrl,
        packageName,
        config.encoding,
        config.protoMeta,
        config.maxResponseSize,
        { timeoutMs: this.timeoutMs, compressRequests: config.compressRequests },
      );
      this.transport = this.connectTransport;
    } else {
      this.transport = new HttpTransport(baseUrl, config.serviceId ?? config.packageName, config.maxResponseSize);
    }

    if (config.clientProtobuf) {
      const protoMeta = protoMetaFromDescriptor(config.clientProtobuf);
      for (const codec of ['proto', 'json'] as const) {
        this.firstPartyConnect.set(
          codec,
          new ConnectTransport(
            baseUrl,
            config.serviceId ?? config.clientProtobuf.package,
            codec,
            protoMeta,
            config.maxResponseSize,
            {
              timeoutMs: this.timeoutMs,
              compressRequests: config.compressRequests ?? false,
            },
          ),
        );
      }
    }

    this.wsTransport = new WebSocketTransport(baseUrl);
    this.sseTransport = new SseTransport(baseUrl, config.serviceId ?? config.packageName ?? '');
    this.serviceWsTransport = new ServiceWebSocketTransport(baseUrl, config.serviceId ?? config.packageName ?? '');
    this.providerWsTransport = new ProviderWebSocketTransport(baseUrl, config.serviceId ?? config.packageName ?? '');

    // Build interceptor chain once: user interceptors → retry → transport.
    // The retry interceptor reads serviceName lazily (at request time) since the
    // subclass field is not yet initialized while this constructor runs. It owns
    // the per-attempt timeout and uses it as the overall retry deadline so the
    // whole sequence stays within a single timeoutMs.
    const userInterceptors = config.interceptors ?? [];
    const attemptTerminal = this.buildChain(config.attemptInterceptors ?? []);
    this.chain = this.buildChain(
      [
        ...userInterceptors,
        retryInterceptor(config.retry, { timeoutMs: this.timeoutMs, getServiceName: () => this.serviceName }),
      ],
      attemptTerminal,
    );

    // Track interceptors that own background resources so dispose() can release
    // them (e.g. circuitBreakerInterceptor's health-probe timer). Without this
    // the timer leaks when the client is discarded while the circuit is open.
    this.disposables = [...userInterceptors, ...(config.attemptInterceptors ?? [])].filter(isDisposableInterceptor);
    this.invalidators = userInterceptors.filter(isResponseCacheInterceptor);

    // Streaming RPCs use WebSocket, not the HTTP transport. Run user
    // interceptors against a synthetic terminal to populate auth/context
    // headers without making a network call or running retry/backoff.
    this.headerChain = this.buildChain(config.streamInterceptors ?? userInterceptors, captureHeaders);
  }

  /**
   * Release background resources held by the client's interceptors.
   *
   * Currently this stops the circuit breaker's health-probe timer (see
   * {@link circuitBreakerInterceptor}). Long-lived processes that recreate
   * clients should call this when discarding a client so the probe timer does
   * not leak. Safe to call multiple times.
   *
   * Also exposed as `[Symbol.dispose]` so the client works with
   * `using client = new FooClient(...)`.
   */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const client of this.endpointClients.values()) client.dispose();
    this.endpointClients.clear();
    for (const stream of this.activeServiceStreams) stream.cancel();
    this.activeServiceStreams.clear();
    for (const breaker of this.streamBreakers.values()) breaker.dispose();
    this.streamBreakers.clear();
    for (const disposable of this.disposables) {
      disposable.dispose();
    }
  }

  /** @internal A generated call selects an immutable, separately resilient target. */
  protected forEndpoint(endpoint: string): this {
    if (this.disposed) throw new CredentialRegistryClosedError();
    if (!this.endpointBinding) {
      throw new ClientServiceConfigError('an endpoint override requires a registered service binding');
    }
    const target = this.endpointBinding(endpoint);
    if (target.url === this.baseUrl) return this;
    let client = this.endpointClients.get(target.url);
    if (!client) {
      client = target.create();
      this.endpointClients.set(target.url, client);
    }
    return client as this;
  }

  /** @internal Turn a synchronous endpoint-selection refusal into a stream terminal. */
  protected failedEndpointStream<TIn, TOut>(error: unknown): DuplexStream<TIn, TOut> & FrameStream<TIn, TOut> {
    const relay = new StreamRelay<TOut>();
    const failure = error instanceof Error ? error : new Error(String(error));
    queueMicrotask(() => relay.fail(failure));
    return {
      ...duplexOf<TIn, TOut>(relay),
      close: () => relay.cancel(),
    };
  }

  /** `using`-statement support — delegates to {@link dispose}. */
  [Symbol.dispose](): void {
    this.dispose();
  }

  /**
   * Drop the cached answers of this client's service whose key starts with
   * `keyPrefix`, for every forwarded identity, and return how many were
   * dropped. A call already in flight for a matching key still answers the
   * callers waiting on it, but its answer is never stored, so the next call
   * goes upstream. An empty prefix drops every answer of the service.
   *
   * The key is `<operationId>?<field>=<value>&…` as ADR 0007 of
   * protocols/clientcontract defines it, identical in the Go runtime: an
   * event subscriber drops one operation with `'effectiveAccess?'` and one
   * principal's answers with `'effectiveAccess?body.principal=%22p1%22'` when
   * `body.principal` is the operation's first key field.
   */
  invalidateResponses(keyPrefix: string): number {
    let dropped = 0;
    for (const invalidator of this.invalidators) dropped += invalidator.invalidateResponses(keyPrefix);
    return dropped;
  }

  /**
   * Drop every cached answer of this client's service whose declared
   * invalidation field carries `value`, across the service's operations and
   * for every forwarded identity, and return how many were dropped. A call in
   * flight for an operation that declares the field still answers the callers
   * waiting on it, but its answer is never stored, so the next call goes
   * upstream.
   *
   * It revokes by a value the request never carried — a principal id the
   * answer names while the request named an issuer and a subject. The provider
   * lists the field in `resilience.cache.invalidationFields` (ADR 0007 of
   * protocols/clientcontract). The value is compared in the canonical form the
   * Go runtime renders too, so the string `'42'` and the integer `42` are
   * different values. A value no runtime can compare — a fraction, an unsafe
   * integer number, null — throws a `TypeError` instead of silently matching
   * nothing. A field no operation declares drops nothing.
   */
  invalidateResponsesByField(field: string, value: ResponseFieldValue): number {
    invalidationTag(field, value);
    let dropped = 0;
    for (const invalidator of this.invalidators) dropped += invalidator.invalidateResponsesByField(field, value);
    return dropped;
  }

  /**
   * Execute a typed request through the interceptor chain.
   *
   * `options.operationId` is the canonical operation id from the API contract —
   * generated methods pass it so the feature trace is resolved from the invoked
   * operation rather than reverse-matched from the URL. It is deliberately not
   * the TypeScript method symbol: a symbol normalizer may rewrite punctuation
   * the canonical id keeps.
   *
   * `options.signal` is the caller's cancel signal. Retry combines it with each
   * attempt's own timeout, so aborting it ends the whole sequence — the
   * in-flight attempt and any pending backoff sleep — instead of only the
   * attempt that happens to be running.
   *
   * `options.withoutResponseCache` asks for the provider's current answer: the
   * operation's declared response cache neither reads, stores nor joins a call
   * in flight, and no stored answer masks a failure.
   *
   * `options.successBody` also receives the declared JSON success body exactly
   * as the provider sent it, once every check of the call accepted it; see
   * {@link SuccessBody}.
   */
  protected async request<T>(
    method: string,
    path: string,
    options?: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      body?: unknown;
      headers?: Record<string, string | undefined>;
      operationId?: string;
      signal?: AbortSignal;
      withoutResponseCache?: boolean;
      successBody?: SuccessBody;
      requestSchema?: ClientSchema;
      requestMediaType?: string;
      streamedRequest?: boolean;
      streamedResponse?: boolean;
      maxPayloadBytes?: number;
      maxRequestBytes?: number;
      successes?: ClientRequest['successes'];
    },
  ): Promise<T> {
    const response = await this.deliverSuccessBody(method, path, options, () =>
      this.executeRequest<T>(method, path, options),
    );
    return response.data;
  }

  /**
   * Run one generated JSON call for a sink, when the call carries one. The sink
   * is refused — and emptied — before anything is sent when this operation
   * declares no JSON success body or the transport that carries it does not
   * keep the body unchanged; otherwise it is held for the duration of the call
   * and filled only with a body the call accepted. A failed call leaves it
   * empty.
   */
  private async deliverSuccessBody<T>(
    method: string,
    path: string,
    options: { operationId?: string; successes?: ClientRequest['successes']; successBody?: SuccessBody } | undefined,
    call: () => Promise<ClientResponse<T>>,
  ): Promise<ClientResponse<T>> {
    const sink = options?.successBody;
    if (!sink) return call();
    const operationId = options?.operationId ?? `${method} ${path}`;
    const operation = options?.operationId ? this.operationContracts?.[options.operationId] : undefined;
    try {
      assertSuccessBodyDeliverable(operation, operationId, options?.successes);
    } catch (error) {
      sink.reset();
      throw error;
    }
    // The transport is the one the declared order routes this call to, decided
    // here exactly as at dispatch, so a transport that does not declare it
    // keeps the body refuses the sink before it runs.
    const routed = this.routedTransport(operation);
    if (!routed.transport.carriesSuccessBody) {
      sink.reset();
      const carrier = routed.declared
        ? `${routed.declared.protocol} with the ${routed.declared.encoding} encoding`
        : 'the configured transport';
      throw new ClientServiceConfigError(
        `operation ${operationId} dispatches on ${carrier}, whose transport does not declare that it carries the success body unchanged; the framework rest-json and connect json transports do`,
      );
    }
    sink.claim();
    let response: ClientResponse<T>;
    try {
      response = await call();
    } catch (error) {
      sink.deliver(undefined);
      throw error;
    }
    if (!response.successBody) {
      // Defensive: the transport declared it keeps the body, but this answer
      // reached the caller without it — an interceptor's, not the provider's.
      sink.deliver(undefined);
      throw new ClientResponseContractError(
        'the answer did not carry the declared success body unchanged',
        this.serviceName,
        operationId,
      );
    }
    sink.deliver(response.successBody);
    return response;
  }

  /**
   * Execute a call whose declared success payload is raw octets.
   *
   * The transport hands the payload over verbatim; this only re-attaches the
   * two facts the caller would otherwise re-read from the wire, and refuses a
   * provider that answered a media type the operation never declared.
   */
  protected async requestBinary<TStatus extends number>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      body?: unknown;
      headers?: Record<string, string | undefined>;
      operationId?: string;
      signal?: AbortSignal;
      withoutResponseCache?: boolean;
      requestMediaType?: string;
      streamedRequest?: boolean;
      maxPayloadBytes?: number;
      maxRequestBytes?: number;
      successes?: ClientRequest['successes'];
      responseMediaType: string;
      successBody?: SuccessBody;
    },
  ): Promise<BinaryPayload<TStatus>> {
    this.refuseSuccessBody(options, `${method} ${path}`);
    const response = await this.executeRequest<Uint8Array>(method, path, options);
    const contentType = (response.headers.get('Content-Type') ?? '').split(';', 1)[0]?.trim() ?? '';
    if (contentType.toLowerCase() !== options.responseMediaType.toLowerCase()) {
      throw new ClientResponseContractError(
        'provider returned an undeclared response content type',
        this.serviceName,
        options.operationId ?? `${method} ${path}`,
      );
    }
    return {
      status: response.status as TStatus,
      contentType,
      body: response.data ?? new Uint8Array(0),
    };
  }

  /** Execute a call while preserving the declared status/header variant. */
  protected async requestBinaryStream<TStatus extends number>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      body?: unknown;
      headers?: Record<string, string | undefined>;
      operationId?: string;
      signal?: AbortSignal;
      requestMediaType?: string;
      requestSchema?: ClientSchema;
      streamedRequest?: boolean;
      streamedResponse: true;
      maxRequestBytes?: number;
      maxPayloadBytes?: number;
      successes?: ClientRequest['successes'];
      successBody?: SuccessBody;
    },
  ): Promise<StreamedBinaryPayload<TStatus>> {
    this.refuseSuccessBody(options, `${method} ${path}`);
    const response = await this.executeRequest<ReadableStream<Uint8Array>>(method, path, options);
    const reader = response.data.getReader();
    let finished = false;
    let sink: ReadableStreamDefaultController<Uint8Array>;
    const release = (): void => {
      finished = true;
      this.activeServiceStreams.delete(handle);
    };
    const handle = {
      cancel: (): void => {
        if (finished) return;
        release();
        sink.error(new CredentialRegistryClosedError());
        void reader.cancel().catch(() => {});
      },
    };
    const body = new ReadableStream<Uint8Array>(
      {
        start(controller) {
          sink = controller;
        },
        async pull(controller) {
          try {
            const item = await reader.read();
            if (finished) return;
            if (item.done) {
              release();
              controller.close();
            } else controller.enqueue(item.value);
          } catch (error) {
            if (!finished) {
              release();
              controller.error(error);
            }
          }
        },
        async cancel(reason) {
          release();
          await reader.cancel(reason);
        },
      },
      { highWaterMark: 0 },
    );
    this.activeServiceStreams.add(handle);
    if (this.disposed) handle.cancel();
    return {
      status: response.status as TStatus,
      contentType: response.headers.get('Content-Type') ?? '',
      body,
    };
  }

  /** Execute a call while preserving the declared status/header variant. */
  protected async requestResult<TResult extends { status: number; data: unknown; headers: Headers }>(
    method: string,
    path: string,
    options?: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      body?: unknown;
      headers?: Record<string, string | undefined>;
      operationId?: string;
      signal?: AbortSignal;
      withoutResponseCache?: boolean;
      successBody?: SuccessBody;
      requestSchema?: ClientSchema;
      requestMediaType?: string;
      streamedRequest?: boolean;
      streamedResponse?: boolean;
      maxPayloadBytes?: number;
      maxRequestBytes?: number;
      successes?: ClientRequest['successes'];
    },
  ): Promise<TResult> {
    const response = await this.deliverSuccessBody(method, path, options, () =>
      this.executeRequest<TResult['data']>(method, path, options),
    );
    // The transport already refused any status the operation did not declare and
    // decoded the body under that status's own schema, so the widened `number`
    // here is known to be one of the result union's status literals.
    return { status: response.status, data: response.data, headers: response.headers } as TResult;
  }

  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: the request boundary normalizes encoding, deadlines, transport failures, and typed responses together
  private async executeRequest<T>(
    method: string,
    path: string,
    options?: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      body?: unknown;
      headers?: Record<string, string | undefined>;
      operationId?: string;
      signal?: AbortSignal;
      withoutResponseCache?: boolean;
      requestSchema?: ClientSchema;
      requestMediaType?: string;
      streamedRequest?: boolean;
      streamedResponse?: boolean;
      maxPayloadBytes?: number;
      maxRequestBytes?: number;
      successes?: ClientRequest['successes'];
    },
  ): Promise<ClientResponse<T>> {
    // A client retained past its application's stop sends nothing: the stop
    // disposed it with the registry it was bound from, so every call through it
    // is refused, anonymous or credentialed, as a stream on it already is.
    if (this.disposed) throw new CredentialRegistryClosedError();
    const headers = new Headers();
    if (options?.headers) {
      for (const [key, value] of Object.entries(options.headers)) {
        if (value !== undefined) headers.set(key, value);
      }
    }

    const mappedPath =
      options?.operationId && this.operationPaths && Object.hasOwn(this.operationPaths, options.operationId)
        ? this.operationPaths[options.operationId]
        : undefined;
    const request: ClientRequest = {
      method,
      path: mappedPath ?? path,
      params: options?.params,
      query: options?.query,
      body: options?.body,
      headers,
      signal: options?.signal,
      ...(options?.withoutResponseCache ? { withoutResponseCache: true } : {}),
      featureTrace: resolveClientFeatureTrace(this.design, options?.operationId),
      ...(options?.operationId ? { operationId: options.operationId } : {}),
      ...(options?.operationId && this.operationContracts?.[options.operationId]
        ? { clientOperation: this.operationContracts[options.operationId] }
        : {}),
      ...(this.clientDefaults ? { clientDefaults: this.clientDefaults } : {}),
      ...(this.clientSchemas ? { clientSchemas: this.clientSchemas } : {}),
      ...(this.carryRemoteMessage ? { carryRemoteMessage: true } : {}),
      ...(options?.requestSchema ? { requestSchema: options.requestSchema } : {}),
      ...(options?.requestMediaType ? { requestMediaType: options.requestMediaType } : {}),
      ...(options?.streamedRequest ? { streamedRequest: true } : {}),
      ...(options?.streamedResponse ? { streamedResponse: true } : {}),
      ...(options?.maxPayloadBytes ? { maxPayloadBytes: options.maxPayloadBytes } : {}),
      ...(options?.maxRequestBytes ? { maxRequestBytes: options.maxRequestBytes } : {}),
      ...(options?.successes ? { successes: options.successes } : {}),
    };
    if (request.streamedRequest || request.streamedResponse) {
      if (request.clientOperation?.resilience?.cache || request.clientDefaults?.cache) {
        throw new ClientRequestEncodingError('a raw HTTP stream cannot declare a response cache');
      }
      request.withoutResponseCache = true;
    }
    const keyHeader = request.clientOperation?.idempotency.keyHeader;
    if (keyHeader && !request.headers.has(keyHeader)) request.headers.set(keyHeader, crypto.randomUUID());
    if (request.clientOperation) request.maxResponseBytes = resolveClientResilience(request).maxResponseBytes;

    try {
      return (await this.chain(request)) as ClientResponse<T>;
    } catch (error) {
      if (request.clientOperation) {
        if (options?.signal?.aborted) throw new ClientCanceledError(this.serviceName, `${method} ${path}`);
        if (error instanceof ClientRetryExhaustedError) {
          throw new ClientFrameworkError({
            service: this.serviceName,
            method: `${method} ${path}`,
            status: 0,
            code: 'client.remote',
          });
        }
        if (error instanceof ClientError) throw error;
        if (error instanceof Error && error.name === 'TimeoutError') {
          throw new ClientDeadlineError(this.serviceName, `${method} ${path}`);
        }
        throw new ClientFrameworkError({
          service: this.serviceName,
          method: `${method} ${path}`,
          status: 0,
          code: 'client.remote',
        });
      }
      // AbortSignal.timeout() surfaces as a DOMException named 'TimeoutError'.
      // Wrap it in the documented typed error carrying the configured timeout.
      if (error instanceof Error && error.name === 'TimeoutError') {
        throw new ClientTimeoutError({
          service: this.serviceName,
          method: `${method} ${path}`,
          timeoutMs: this.timeoutMs,
          cause: error,
        });
      }
      throw error;
    }
  }

  /**
   * Open a server-streaming connection.
   *
   * On a Connect client the stream is attempted over Connect first — the
   * protocol carries server streams fine over HTTP/1.1 as envelope frames — and
   * falls back to WebSocket when the server answers gRPC status 12
   * `UNIMPLEMENTED` (the status the Connect route reports for a mode it cannot
   * serve) or answers `200` with something that is not a Connect stream. Any
   * other failure stays typed rather than being masked by the fallback. HTTP
   * clients go straight to WebSocket. Transport choice stays automatic; callers
   * see the same {@link StreamObserver} either way.
   *
   * Headers are populated by running the same user interceptor chain used for
   * unary requests so streaming calls carry auth/context/trace headers.
   */
  protected stream<T>(path: string, options?: { body?: unknown }): StreamObserver<T> {
    const headers = this.prepareStreamHeaders(path);
    if (!this.connectTransport) {
      return this.openWebSocketStream<T>(path, options?.body, headers);
    }

    const relay = new StreamRelay<T>();
    this.connectTransport.streamInto<T>(relay, path, {
      body: options?.body,
      headers,
      onUnimplemented: () => {
        if (!isWebSocketAvailable()) {
          relay.fail(this.streamTransportUnavailable(path));
          return true;
        }
        relay.adopt(this.wsTransport.stream<T>(path, options?.body, headers));
        return true;
      },
    });
    return relay;
  }

  /**
   * Open a generated first-party server stream over the provider's declared
   * transports, in the provider's own order.
   *
   * The declared order is the dispatch order: the first transport this runtime
   * can carry is opened first, and the caller never branches on it. When — and
   * only when — the provider answers that this transport is not served at this
   * path, the next declared transport is opened. The generated method signature
   * is the same whichever transport carries the stream, which is precisely why
   * the fallback belongs here and not in a second generated method. Everything
   * else about the lifecycle belongs to the {@link StreamSession}.
   */
  protected serviceStream<T>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
      successBody?: SuccessBody;
    },
  ): StreamObserver<T> {
    const relay = new TrackedStreamRelay<T>(() => this.activeServiceStreams.delete(relay));
    this.activeServiceStreams.add(relay);
    if (this.disposed) {
      queueMicrotask(() => relay.fail(new ClientCanceledError(this.serviceName, options.operationId)));
      return relay;
    }
    const sinkRefusal = this.successBodyRefusal(options, options.operationId);
    if (sinkRefusal) {
      queueMicrotask(() => relay.fail(sinkRefusal));
      return relay;
    }
    const operation = this.operationContracts?.[options.operationId];
    const candidates = operation
      ? streamTransportCandidates(operation, 'server', (encoding) => this.firstPartyConnect.has(encoding))
      : [];
    if (operation?.stream !== 'server' || !operation.messages?.output || candidates.length === 0) {
      queueMicrotask(() => relay.fail(this.streamTransportUnavailable(path, 'sse')));
      return relay;
    }
    // A fallback re-opens the operation. That is only sound where re-opening
    // cannot repeat an effect, so an operation the provider did not declare
    // replayable gets exactly one attempt on its first declared transport.
    const declared = streamFallbackAllowed(operation) ? candidates : candidates.slice(0, 1);
    this.openDeclaredServerStream<T>(relay, method, path, options, operation, declared, 0);
    return relay;
  }

  /**
   * Open one declared transport of a server stream and, on an answer that says
   * this wire is not served here, hand the caller's relay to the next one.
   *
   * Each attempt owns its own session: the one that admits is the one that
   * measures the call, and an attempt the provider never admitted contributes
   * its own pre-admission verdict and nothing else.
   */
  private openDeclaredServerStream<T>(
    relay: TrackedStreamRelay<T>,
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
    },
    operation: ClientContractOperation,
    candidates: readonly ClientTransportContract[],
    index: number,
  ): void {
    const transport = candidates[index] as ClientTransportContract;
    const { session, request } = this.openServiceSession<T>(method, path, options, 'server', transport, operation);
    const observer = session.observer();
    let handedOver = false;
    const forward = (): void => observer.onMessage((value) => relay.deliver(value));
    if (transport.protocol === 'sse' && transport.sse?.continuation) {
      // A declared continuation resumes after the last value the caller
      // received (clientcontract ADR 0013), so the session hands values over
      // only once the caller can receive them: a value retained for a caller
      // who has not subscribed yet was not received, and the position never
      // includes it.
      relay.onSubscribe(forward);
    } else {
      forward();
    }
    observer.onComplete(() => relay.complete());
    observer.onError((error) => {
      // Nothing was delivered: the provider refused before admission, and its
      // refusal says this wire is not there. Anything else is a fact about the
      // call, and asking a second wire would only ask it again.
      if (!handedOver && index + 1 < candidates.length && !session.wasAdmitted && isTransportUnavailable(error)) {
        handedOver = true;
        this.openDeclaredServerStream(relay, method, path, options, operation, candidates, index + 1);
        return;
      }
      relay.fail(error);
    });
    relay.attach(() => observer.cancel());

    const output = operation.messages?.output as ClientSchema;
    void this.headerChain(request).then(
      () => {
        if (session.isCancelled || relay.isCancelled) return;
        if (transport.protocol === 'connect') {
          const connect = this.connectFor(transport.encoding);
          if (!connect) {
            session.fail(this.streamTransportUnavailable(path, 'connect'));
            session.close();
            return;
          }
          connect.openServiceStream<T>(request, session, { output });
          return;
        }
        if (transport.protocol === 'sse') {
          const continuation = transport.sse?.continuation;
          this.sseTransport.stream<T>(
            request,
            continuation
              ? {
                  output,
                  continuation,
                  // Automatic continuation needs a safe stream, the provider's
                  // declaration and effective reconnect (ADR 0013).
                  reconnect: operation.idempotency.kind === 'safe' && resolveClientResilience(request).stream.reconnect,
                }
              : { output },
            session,
          );
          return;
        }
        this.serviceWsTransport.stream<T>(
          request,
          {
            stream: 'server',
            output,
            resumeDeclared: transport.websocket?.resume ?? false,
            reconnectDeclared: resolveClientResilience(request).stream.reconnect,
          },
          session,
        );
      },
      (error) => {
        session.fail(error);
        session.close();
      },
    );
  }

  /**
   * Open a generated first-party client stream: the caller sends messages, the
   * provider answers with exactly one declared value.
   */
  protected serviceClientStream<TIn, TOut>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
      successBody?: SuccessBody;
    },
  ): DuplexStream<TIn, TOut> {
    return this.openServiceDuplex<TIn, TOut>(method, path, options, 'client');
  }

  /** Open a generated first-party bidirectional stream. */
  protected serviceBidiStream<TIn, TOut>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
      successBody?: SuccessBody;
    },
  ): DuplexStream<TIn, TOut> {
    return this.openServiceDuplex<TIn, TOut>(method, path, options, 'bidirectional');
  }

  private openServiceDuplex<TIn, TOut>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
      successBody?: SuccessBody;
    },
    stream: 'client' | 'bidirectional',
  ): DuplexStream<TIn, TOut> {
    const sinkRefusal = this.successBodyRefusal(options, options.operationId);
    if (sinkRefusal) return this.failedEndpointStream<TIn, TOut>(sinkRefusal);
    const opened = this.openServiceStream<TIn, TOut>(method, path, options, stream);
    if (!opened.session) return duplexOf(opened.failed as StreamObserver<TOut>);
    const { session, request, transport, operation } = opened;
    // The socket is not opened until the interceptor chain has produced the
    // credentials and context the init frame carries, so nothing is sent
    // anonymously and the queued frames leave in caller order.
    let live: DuplexStream<TIn, TOut> | undefined;
    const queued: TIn[] = [];
    let ended = false;
    void this.headerChain(request).then(
      () => {
        if (session.isCancelled) return;
        const duplex = this.serviceWsTransport.streamDuplex<TIn, TOut>(
          request,
          {
            stream,
            input: operation.messages?.input as ClientSchema,
            output: operation.messages?.output as ClientSchema,
            // A duplex conversation is never continued: the caller's own
            // messages would travel twice.
            resumeDeclared: transport.websocket?.resume ?? false,
            reconnectDeclared: false,
          },
          session,
        );
        live = duplex;
        for (const value of queued.splice(0)) duplex.send(value);
        if (ended) duplex.end();
      },
      (error) => {
        session.fail(error);
        session.close();
      },
    );
    const observer = session.observer();
    return {
      onMessage: (handler) => observer.onMessage(handler),
      onError: (handler) => observer.onError(handler),
      onComplete: (handler) => observer.onComplete(handler),
      cancel: () => observer.cancel(),
      send: (value) => {
        if (ended) return;
        if (live) live.send(value);
        else queued.push(value);
      },
      end: () => {
        ended = true;
        live?.end();
      },
    };
  }

  /** The first-party Connect transport of one declared encoding; raw octets have none. */
  private connectFor(encoding: ClientTransportContract['encoding']): ConnectTransport | undefined {
    return encoding === 'binary' ? undefined : this.firstPartyConnect.get(encoding);
  }

  /**
   * Open a generated byte stream over the operation's provider-owned wire.
   *
   * The upgrade request is the admission, so it is built exactly like a unary
   * call — the interceptor chain applies the declared credentials, the client
   * identity and trace context — and the socket is opened with it. The promise
   * resolves once the provider accepted the upgrade; a refusal, a budget or a
   * shape this runtime cannot carry rejects it with the typed error.
   */
  protected serviceByteStream(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
      successBody?: SuccessBody;
    },
  ): Promise<ByteStream> {
    const sinkRefusal = this.successBodyRefusal(options, options.operationId);
    if (sinkRefusal) return Promise.reject(sinkRefusal);
    const opened = this.openProviderWire<Uint8Array>(method, path, options, true);
    if (!opened.session) return Promise.reject(opened.failure);
    const { session, request, transport } = opened;
    return this.headerChain(request).then(
      () => this.providerWsTransport.openBytes(request, { transport }, session),
      (error) => {
        const surfaced = session.fail(error);
        session.close();
        throw surfaced;
      },
    );
  }

  /**
   * Open a generated stream of typed JSON frames over the operation's
   * provider-owned wire, under the subprotocol the provider declares. Frames
   * sent before admission are queued in caller order.
   */
  protected serviceFrameStream<TIn, TOut>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
      successBody?: SuccessBody;
    },
  ): FrameStream<TIn, TOut> {
    const sinkRefusal = this.successBodyRefusal(options, options.operationId);
    if (sinkRefusal) return this.failedEndpointStream<TIn, TOut>(sinkRefusal);
    const opened = this.openProviderWire<TOut>(method, path, options, false);
    if (!opened.session) {
      const relay = new StreamRelay<TOut>();
      queueMicrotask(() => relay.fail(opened.failure));
      return { ...frameObserverOf(relay), send: () => undefined, close: () => relay.cancel() };
    }
    const { session, request, transport, operation } = opened;
    let live: FrameStream<TIn, TOut> | undefined;
    const queued: TIn[] = [];
    let closed = false;
    void this.headerChain(request).then(
      () => {
        if (session.isCancelled) return;
        live = this.providerWsTransport.openFrames<TIn, TOut>(
          request,
          {
            transport,
            input: operation.messages?.input as ClientSchema,
            output: operation.messages?.output as ClientSchema,
          },
          session,
        );
        for (const frame of queued.splice(0)) live.send(frame);
        if (closed) live.close();
      },
      (error) => {
        session.fail(error);
        session.close();
      },
    );
    return {
      ...frameObserverOf(session.observer()),
      send: (frame) => {
        if (closed) return;
        if (live) live.send(frame);
        else queued.push(frame);
      },
      close: () => {
        closed = true;
        if (live) live.close();
      },
    };
  }

  /**
   * Resolve the operation, its provider-owned wire and the session one opening
   * needs. A shape this runtime cannot carry fails before any socket opens,
   * with the transport named.
   */
  private openProviderWire<TOut>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
    },
    bytes: boolean,
  ):
    | {
        session: StreamSession<TOut>;
        request: ClientRequest;
        transport: ClientTransportContract;
        operation: ClientContractOperation;
        failure?: undefined;
      }
    | { session?: undefined; failure: Error } {
    if (this.disposed) return { failure: new ClientCanceledError(this.serviceName, options.operationId) };
    const operation = this.operationContracts?.[options.operationId];
    const transport = operation?.transports.find((candidate) => candidate.websocket?.wire === 'provider');
    const carried =
      operation?.stream === 'bidirectional' &&
      transport !== undefined &&
      (bytes
        ? transport.encoding === 'binary'
        : transport.encoding === 'json' && !!operation.messages?.input && !!operation.messages?.output);
    if (!operation || !transport || !carried) {
      return { failure: this.streamTransportUnavailable(path, 'websocket') };
    }
    return {
      ...this.openServiceSession<TOut>(method, path, options, 'bidirectional', transport, operation),
      transport,
      operation,
    };
  }

  /**
   * Resolve the operation, the transport and the session one stream needs.
   *
   * A declared shape this runtime cannot carry fails before any socket opens,
   * with the transport named — never with a partially opened stream.
   */
  private openServiceStream<_TIn, TOut>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
    },
    stream: 'client' | 'bidirectional',
  ):
    | {
        session: StreamSession<TOut>;
        request: ClientRequest;
        transport: ClientTransportContract;
        operation: ClientContractOperation;
        failed?: undefined;
      }
    | { session?: undefined; failed: StreamObserver<TOut> } {
    const relay = new TrackedStreamRelay<TOut>(() => this.activeServiceStreams.delete(relay));
    this.activeServiceStreams.add(relay);
    const operation = this.operationContracts?.[options.operationId];
    // A client or bidirectional stream never falls back: the caller's own
    // messages would travel twice. It opens the first declared transport this
    // runtime can carry, or nothing.
    const transport = operation
      ? streamTransportCandidates(operation, stream, (encoding) => this.firstPartyConnect.has(encoding))[0]
      : undefined;
    if (this.disposed) {
      queueMicrotask(() => relay.fail(new ClientCanceledError(this.serviceName, options.operationId)));
      return { failed: relay };
    }
    if (!operation || operation.stream !== stream || !operation.messages?.output || !transport) {
      queueMicrotask(() => relay.fail(this.streamTransportUnavailable(path, 'websocket')));
      return { failed: relay };
    }
    if (!operation.messages.input) {
      queueMicrotask(() => relay.fail(this.streamTransportUnavailable(path, 'websocket')));
      return { failed: relay };
    }
    this.activeServiceStreams.delete(relay);
    return {
      ...this.openServiceSession<TOut>(method, path, options, stream, transport, operation),
      transport,
      operation,
    };
  }

  /**
   * Build the request and the session one attempt on one declared transport
   * needs. The transport is decided by the caller, because the declared order
   * is what decides it.
   */
  private openServiceSession<TOut>(
    method: string,
    path: string,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string | readonly string[] | undefined>;
      headers?: Record<string, string | undefined>;
      operationId: string;
      signal?: AbortSignal;
    },
    stream: 'server' | 'client' | 'bidirectional',
    transport: ClientTransportContract,
    operation: ClientContractOperation,
  ): { session: StreamSession<TOut>; request: ClientRequest } {
    void path;
    void stream;
    const headers = new Headers();
    for (const [name, value] of Object.entries(options.headers ?? {})) {
      if (value !== undefined) headers.set(name, value);
    }
    // The credential-free header set every resolution starts from. A
    // credential re-applied over the one a previous connection carried is
    // refused by the credential rules, so a re-resolution rebuilds from here.
    const pristine = new Headers(headers);
    const request: ClientRequest = {
      method,
      path: transport.path,
      params: options.params,
      query: options.query,
      headers,
      signal: options.signal,
      operationId: options.operationId,
      clientOperation: operation,
      ...(this.clientDefaults ? { clientDefaults: this.clientDefaults } : {}),
      ...(this.clientSchemas ? { clientSchemas: this.clientSchemas } : {}),
      ...(this.carryRemoteMessage ? { carryRemoteMessage: true } : {}),
    };
    const resilience = resolveClientResilience(request);
    const protocol = transport.protocol === 'sse' ? 'sse' : transport.protocol === 'connect' ? 'connect' : 'websocket';
    const session = new StreamSession<TOut>({
      serviceId: this.serviceName,
      operationId: options.operationId,
      protocol,
      budgets: streamBudgets(resilience),
      breaker: this.streamBreaker(options.operationId, resilience),
      startCall: () => {
        const finish = startServiceStreamCall(this.serviceName, request, protocol);
        return (result) => finish({ error: result.error, status: result.status });
      },
      credentials: async () => {
        request.headers = new Headers(pristine);
        request.credentialValues = undefined;
        request.credentialHeaderNames = undefined;
        await this.headerChain(request);
      },
      invalidateCredentials: () => rejectRequestCredentials(request),
      mapError: (error) => this.normalizeStreamError(error, request),
      ...(options.signal ? { callerSignal: options.signal } : {}),
    });
    const tracked = { cancel: () => session.observer().cancel() };
    this.activeServiceStreams.add(tracked);
    session.observer().onComplete(() => this.activeServiceStreams.delete(tracked));
    session.observer().onError(() => this.activeServiceStreams.delete(tracked));
    return { session, request };
  }

  /** The circuit this operation shares across its streams. */
  private streamBreaker(operationId: string, resilience: ReturnType<typeof resolveClientResilience>): CircuitBreaker {
    const existing = this.streamBreakers.get(operationId);
    if (existing) return existing;
    const breaker = new CircuitBreaker({
      failureThreshold: resilience.circuit.failureThreshold,
      resetTimeoutMs: resilience.circuit.resetTimeoutMs,
      failureStatuses: [500, 502, 503, 504],
      serviceName: this.serviceName,
    });
    this.streamBreakers.set(operationId, breaker);
    return breaker;
  }

  /**
   * Open a bidirectional streaming connection via WebSocket.
   *
   * Client- and bidi-streaming cannot ride Connect over HTTP/1.1, so these
   * modes go straight to WebSocket rather than paying a round trip to be told
   * `UNIMPLEMENTED`. When the runtime has no WebSocket the stream fails with a
   * typed {@link ClientTransportUnavailableError} instead of a bare
   * `ReferenceError`.
   *
   * Headers are populated through the user interceptor chain; see {@link stream}.
   */
  protected streamDuplex<TIn, TOut>(path: string, options?: { body?: unknown }): DuplexStream<TIn, TOut> {
    if (!isWebSocketAvailable()) {
      return unavailableDuplex<TIn, TOut>(this.streamTransportUnavailable(path));
    }
    return this.wsTransport.streamDuplex<TIn, TOut>(path, options?.body, this.prepareStreamHeaders(path));
  }

  private openWebSocketStream<T>(path: string, body: unknown, headers: Promise<Headers>): StreamObserver<T> {
    if (!isWebSocketAvailable()) {
      const relay = new StreamRelay<T>();
      queueMicrotask(() => relay.fail(this.streamTransportUnavailable(path)));
      return relay;
    }
    return this.wsTransport.stream<T>(path, body, headers);
  }

  private streamTransportUnavailable(path: string, transport = 'websocket'): ClientTransportUnavailableError {
    return new ClientTransportUnavailableError({
      service: this.serviceName,
      method: path,
      transport,
    });
  }

  private normalizeStreamError(error: unknown, request: ClientRequest): Error {
    if (error instanceof ClientError) return error;
    if (request.signal?.aborted) return new ClientCanceledError(this.serviceName, request.operationId ?? request.path);
    if (error instanceof Error && error.name === 'TimeoutError') {
      return new ClientDeadlineError(this.serviceName, request.operationId ?? request.path);
    }
    return new ClientFrameworkError({
      service: this.serviceName,
      method: request.operationId ?? request.path,
      status: 0,
      code: 'client.remote',
    });
  }

  private async prepareStreamHeaders(path: string): Promise<Headers> {
    const request: ClientRequest = {
      method: 'GET',
      path,
      headers: new Headers(),
    };
    const response = await this.headerChain(request);
    return response.headers;
  }

  /**
   * Build the interceptor chain once at construction time.
   * The chain is a single function that wraps a terminal handler with interceptors.
   * The terminal defaults to the HTTP/Connect transport.
   */
  /**
   * The transport one request travels on.
   *
   * A first-party operation carries its provider's ordered transport list, and
   * the first entry this runtime can carry wins — so a provider that declares
   * `connect` before `rest-json` is answered over Connect without the consumer
   * branching, and the whole interceptor chain (credentials, deadline, retry,
   * circuit, telemetry) is the same either way. A call with no contract keeps
   * the transport the client was configured with.
   */
  /**
   * A call that cannot deliver a success body — raw octets, a stream — refuses
   * a sink before it sends anything, so the caller never holds an earlier
   * call's bytes believing they are this one's.
   */
  private refuseSuccessBody(options: { successBody?: SuccessBody } | undefined, operation: string): void {
    const refusal = this.successBodyRefusal(options, operation);
    if (refusal) throw refusal;
  }

  private successBodyRefusal(
    options: { successBody?: SuccessBody } | undefined,
    operation: string,
  ): ClientServiceConfigError | undefined {
    if (options?.successBody === undefined) return undefined;
    // A refused call delivers no bytes, and never leaves an earlier call's
    // body for the caller to mistake for its own.
    options.successBody.reset();
    return new ClientServiceConfigError(
      `operation ${operation} does not declare a JSON success body; only a generated call that does delivers it into a success body sink`,
    );
  }

  /**
   * The transport the declared order routes a unary operation to: the first
   * declared transport this runtime can carry, or the configured one when the
   * operation declares none it can. `declared` is the contract entry that
   * chose it, absent for the configured fallback.
   */
  private routedTransport(operation: ClientContractOperation | undefined): {
    transport: Transport;
    declared?: ClientTransportContract;
  } {
    if (operation?.stream !== 'unary') return { transport: this.transport };
    for (const declared of operation.transports) {
      if (declared.protocol === 'rest-json') return { transport: this.transport, declared };
      if (declared.protocol !== 'connect') continue;
      const connect = this.connectFor(declared.encoding);
      if (connect) return { transport: connect, declared };
    }
    return { transport: this.transport };
  }

  private routeRequest(request: ClientRequest): { transport: Transport; request: ClientRequest } {
    const routed = this.routedTransport(request.clientOperation);
    if (routed.declared?.protocol !== 'connect') return { transport: routed.transport, request };
    // The Connect path is the RPC path the provider published, not the REST
    // one. It is applied to a copy so the request the interceptors saw — and
    // the key the credential registry recorded it under — is left alone.
    return { transport: routed.transport, request: { ...request, path: routed.declared.path } };
  }

  private buildChain(
    interceptors: Interceptor[],
    terminal: (req: ClientRequest) => Promise<ClientResponse> = (req) => {
      const routed = this.routeRequest(req);
      return routed.transport.execute(routed.request);
    },
  ): (req: ClientRequest) => Promise<ClientResponse> {
    let chain = terminal;
    for (let i = interceptors.length - 1; i >= 0; i--) {
      const interceptor = interceptors[i];
      const next = chain;
      chain = (req) => interceptor(req, next);
    }
    return chain;
  }
}

/** The observer half of a frame stream, delegating to the session's relay. */
function frameObserverOf<T>(observer: StreamObserver<T>): StreamObserver<T> {
  return {
    onMessage: (handler) => observer.onMessage(handler),
    onError: (handler) => observer.onError(handler),
    onComplete: (handler) => observer.onComplete(handler),
    cancel: () => observer.cancel(),
  };
}

class TrackedStreamRelay<T> extends StreamRelay<T> {
  constructor(private readonly release: () => void) {
    super();
  }

  override cancel(): void {
    super.cancel();
    this.release();
  }

  override fail(error: Error): void {
    super.fail(error);
    this.release();
  }

  override complete(): void {
    super.complete();
    this.release();
  }
}

/** Default per-request timeout in ms when none is configured. */
const DEFAULT_TIMEOUT_MS = 30_000;

/** Upper bound on a configured per-request timeout (10 minutes). */
const MAX_TIMEOUT_MS = 10 * 60_000;

function resolveTimeoutMs(timeoutMs?: number): number {
  if (timeoutMs === undefined) return DEFAULT_TIMEOUT_MS;
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
    throw new Error(`ClientConfig: timeoutMs must be a positive number, got ${timeoutMs}`);
  }
  return Math.min(timeoutMs, MAX_TIMEOUT_MS);
}

async function captureHeaders(req: ClientRequest): Promise<ClientResponse> {
  return { data: undefined, status: 0, headers: req.headers };
}

/** True when this runtime exposes a `WebSocket` constructor. */
function isWebSocketAvailable(): boolean {
  return typeof globalThis.WebSocket !== 'undefined';
}

/**
 * A duplex stream that only ever reports why it could not be opened.
 *
 * The failure is delivered on a microtask so the caller's `onError` — always
 * registered synchronously after the call — is in place to receive it.
 */
function unavailableDuplex<TIn, TOut>(error: Error): DuplexStream<TIn, TOut> {
  const relay = new StreamRelay<TOut>();
  queueMicrotask(() => relay.fail(error));
  return {
    onMessage: (handler) => relay.onMessage(handler),
    onError: (handler) => relay.onError(handler),
    onComplete: (handler) => relay.onComplete(handler),
    send: () => {},
    end: () => {},
    cancel: () => relay.cancel(),
  };
}

/**
 * The declared transports this runtime can carry, in the provider's own order.
 * Choosing anything else would make the consumer, not the provider, the
 * authority on how an operation travels.
 *
 * A protocol declared twice yields a single candidate: each attempt opens the
 * first transport of its own protocol, so a second declaration of the same
 * protocol would re-open the same wire.
 */
function streamTransportCandidates(
  operation: ClientContractOperation,
  stream: 'server' | 'client' | 'bidirectional',
  canConnect: (encoding: ConnectEncoding) => boolean,
): ClientTransportContract[] {
  const seen = new Set<string>();
  return operation.transports.filter((transport) => {
    if (seen.has(transport.protocol)) return false;
    // Connect carries a server stream as envelope frames over one POST; it
    // cannot carry a client or bidirectional stream over HTTP/1.1.
    let carriable: boolean;
    if (transport.protocol === 'connect')
      carriable = stream === 'server' && transport.encoding !== 'binary' && canConnect(transport.encoding);
    else if (transport.protocol === 'sse') carriable = stream === 'server';
    // A provider-owned wire is never the first-party conversation: it has its
    // own entry points and is not a candidate here.
    else if (transport.protocol === 'websocket')
      carriable = isWebSocketAvailable() && transport.encoding === 'json' && transport.websocket?.wire === undefined;
    else carriable = false;
    if (carriable) seen.add(transport.protocol);
    return carriable;
  });
}

/**
 * Whether this operation may be opened a second time on another declared
 * transport.
 *
 * Only a declared-replayable operation may: a fallback is a second opening, and
 * an operation whose provider did not state that re-running it is harmless must
 * never be re-run because a wire looked absent.
 */
function streamFallbackAllowed(operation: ClientContractOperation): boolean {
  return operation.idempotency.kind === 'safe' || operation.idempotency.kind === 'idempotent';
}

/**
 * The one failure class a declared fallback acts on: the provider answered, and
 * its answer says this transport is not served at this path.
 *
 * 404, 405 and 426 are the three HTTP ways to say it, gRPC status 12 is the
 * Connect one, and a handshake that did not select the first-party subprotocol
 * is the WebSocket one. Everything else — a refused credential, a server fault,
 * a rate limit, a budget, a contract violation — says something about the call.
 */
function isTransportUnavailable(error: unknown): boolean {
  if (error instanceof ClientTransportUnavailableError) return true;
  if (!(error instanceof ClientError)) return false;
  if (error.grpcCode === 12) return true;
  return error.status === 404 || error.status === 405 || error.status === 426;
}

/** Project the merged provider policy onto the four session bounds. */
function streamBudgets(resilience: ReturnType<typeof resolveClientResilience>): StreamBudgets {
  return {
    handshakeMs: resilience.stream.handshakeTimeoutMs,
    idleMs: resilience.stream.idleTimeoutMs,
    // The declared operation duration bounds the whole session only when it is
    // declared; absent, the handshake, idle and frame budgets still apply.
    sessionMs: 0,
    heartbeatMs: resilience.stream.heartbeatMs,
    maxFrameBytes: resilience.stream.maxFrameBytes,
    maxBufferedMessages: resilience.stream.maxBufferedMessages,
  };
}

/** A duplex that only ever reports why it could not be opened. */
function duplexOf<TIn, TOut>(observer: StreamObserver<TOut>): DuplexStream<TIn, TOut> {
  return {
    onMessage: (handler) => observer.onMessage(handler),
    onError: (handler) => observer.onError(handler),
    onComplete: (handler) => observer.onComplete(handler),
    cancel: () => observer.cancel(),
    send: () => {},
    end: () => {},
  };
}
