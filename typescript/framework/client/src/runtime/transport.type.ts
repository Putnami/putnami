import type { ClientContractOperation, ClientResiliencePolicy, ClientSchema } from '@putnami/application';
import type { ClientFeatureTrace } from './generated-client-design';

/**
 * Represents an outgoing client request before it hits the transport.
 */
export interface ClientRequest {
  /** HTTP method (GET, POST, PUT, DELETE, PATCH) or Connect RPC method */
  method: string;
  /** URL path template with {param} placeholders */
  path: string;
  /** Path parameters to substitute into the path template */
  params?: Record<string, string>;
  /** Query parameters */
  query?: Record<string, string | readonly string[] | undefined>;
  /** Request body (will be JSON-serialized for HTTP, proto-encoded for Connect) */
  body?: unknown;
  /** Request headers — interceptors add to these */
  headers: Headers;
  /** Abort signal for cancellation/timeout */
  signal?: AbortSignal;
  /** Exact generated-client → operation → producer-feature identity, when available. */
  featureTrace?: ClientFeatureTrace;
  /** @internal Immutable first-party operation metadata embedded by the generator. */
  clientOperation?: ClientContractOperation;
  /** Canonical operation id embedded by the generator. */
  operationId?: string;
  /** @internal Provider document defaults merged before operation overrides. */
  clientDefaults?: ClientResiliencePolicy;
  /** @internal Neutral component schemas used to resolve declared error refs. */
  clientSchemas?: Readonly<Record<string, ClientSchema>>;
  /** @internal Absolute total deadline shared by auth, retries, and transport. */
  deadlineAt?: number;
  /**
   * @internal The response cache's shared call: it answers every caller
   * waiting on one key, so no single caller's ambient cancellation or deadline
   * bounds it. Only the operation's declared budget and `signal` (the
   * registry's end) do.
   */
  detachedFromCaller?: boolean;
  /**
   * The caller asked for the provider's current answer: the declared response
   * cache neither reads, stores nor joins a call in flight for this request.
   */
  withoutResponseCache?: boolean;
  /** @internal Active credentials used only to redact declared error details. */
  secretValues?: string[];
  /**
   * @internal The binding opted in to carrying the provider's free-text error
   * `message` onto the typed error (`ServiceBinding.carryRemoteMessage`).
   * Absent means no: a request no client stamped never carries provider prose.
   */
  carryRemoteMessage?: boolean;
  /** @internal Selected credential profile values for protected WS init frames. */
  credentialValues?: { profile: string; value: string }[];
  /** @internal Credential header names excluded from ordinary WS init headers. */
  credentialHeaderNames?: string[];
  /**
   * @internal The one-shot re-mint of the forwarded-user credential the last
   * resolution applied, when its binding declares one. A stream reopening the
   * provider refuses with 401 uses it once, the way a unary attempt does.
   */
  forwardedUserRefresh?: (signal?: AbortSignal) => Promise<string>;
  /** @internal Per-operation response cap after provider policy merge. */
  maxResponseBytes?: number;
  /**
   * @internal Bound of a declared success payload, and only a successful one.
   * It carries the contract's own bound — today, a raw octet declaration —
   * which is usually far smaller than the resilience cap. Applying it to an
   * error body too would make a provider's own refusal unreadable, so it
   * narrows the read on 2xx and nowhere else.
   */
  maxPayloadBytes?: number;
  /** @internal Declared bound enforced incrementally for a streamed request. */
  maxRequestBytes?: number;
  /**
   * @internal Media type of a raw octet request payload. When set, the body is
   * already octets: the transport sends them verbatim under this type instead
   * of encoding a JSON document.
   */
  requestMediaType?: string;
  /** @internal Raw HTTP body ownership; uploads cannot be replayed. */
  streamedRequest?: boolean;
  /** @internal Return the native response reader without a whole-body read. */
  streamedResponse?: boolean;
  /** @internal Request JSON schema embedded by a first-party method. */
  requestSchema?: ClientSchema;
  /** @internal Complete successful HTTP variants embedded by a first-party method. */
  successes?: readonly ClientSuccessDescriptor[];
  /** @internal Shared per-call state updated by the retry loop. */
  telemetryState?: { attempts: number };
  /**
   * Per-attempt slot the transport fills with the provider's `Retry-After`
   * budget, in milliseconds. The retry interceptor sits outside the transport
   * and never sees response headers on the throwing path, so the hint travels
   * here instead of on the error.
   */
  retryState?: { retryAfterMs?: number };
  /** @internal One-based attempt number for generated-client telemetry. */
  telemetryAttempt?: number;
  /** @internal Credential acquisition duration for this attempt. */
  authDurationMs?: number;
}

interface ClientSuccessDescriptor {
  status: number;
  description: string;
  content: readonly { mediaType: string; schema?: ClientSchema; maxBytes?: number; streamed?: boolean }[];
  headers?: readonly { name: string; required: boolean; schema: ClientSchema }[];
}

/**
 * Represents the response from a client call.
 */
export interface ClientResponse<T = unknown> {
  /** Parsed response body */
  data: T;
  /** HTTP status code */
  status: number;
  /** Response headers */
  headers: Headers;
  /**
   * @internal The declared JSON success body exactly as the provider sent it,
   * set by a transport that carried it unchanged and decoded `data` from it.
   * Absent on every other answer.
   */
  successBody?: Uint8Array;
}

/**
 * Transport interface — the lowest layer that actually sends the request.
 * HTTP and Connect are the two implementations.
 */
export interface Transport {
  execute<T>(request: ClientRequest): Promise<ClientResponse<T>>;
  /**
   * Declared by a transport whose `execute` sets `ClientResponse.successBody`
   * to the declared JSON success body exactly as the provider sent it. A
   * transport that does not declare it cannot deliver a success body, so a
   * call that asks for one is refused before the transport runs.
   */
  readonly carriesSuccessBody?: boolean;
}

/**
 * Interceptor function — wraps the transport call.
 * Each interceptor can modify the request, observe the response, or short-circuit.
 */
export type Interceptor = (
  request: ClientRequest,
  next: (request: ClientRequest) => Promise<ClientResponse>,
) => Promise<ClientResponse>;

/**
 * An {@link Interceptor} that owns a resource (e.g. a background timer) and
 * therefore carries a `dispose()` to release it. {@link BaseClient} discovers
 * disposable interceptors and tears them down from `BaseClient.dispose()`.
 */
export type DisposableInterceptor = Interceptor & { dispose: () => void };

/** Narrow an interceptor to a {@link DisposableInterceptor}. */
export function isDisposableInterceptor(interceptor: Interceptor): interceptor is DisposableInterceptor {
  return typeof (interceptor as Partial<DisposableInterceptor>).dispose === 'function';
}
