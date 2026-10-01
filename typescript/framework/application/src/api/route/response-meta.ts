import type { SchemaDefinition } from '@putnami/runtime';
import type { SecurityOptions } from '../../security/security.types';
import type { ClientOperationPolicy } from '../client-contract';
import type { HttpCacheOptions } from '../../http/cache.middleware';
import type { CorsOptions } from '../../http/cors.middleware';

/**
 * Describes a single response entry: status code + optional description + optional schema.
 * Used for both success returns (`.returns()`) and error throws (`.throws()`).
 */
export interface ResponseMeta {
  readonly status: number;
  readonly description?: string;
  readonly schema?: SchemaDefinition;
}

/**
 * Framework-known error codes that can be mapped to standard OpenAPI error responses.
 *
 * These are documentation hints only: they describe what a handler may throw and do not
 * change runtime behavior. Use `.throws()` when a route needs a custom status, description,
 * or schema.
 */
export type ErrorResponseCode =
  | 'BadRequest'
  | 'Validation'
  | 'InvalidArgument'
  | 'Unauthorized'
  | 'Forbidden'
  | 'NotFound'
  | 'MethodNotAllowed'
  | 'NotAcceptable'
  | 'ProxyAuthenticationRequired'
  | 'RequestTimeout'
  | 'Conflict'
  | 'AlreadyExists'
  | 'Gone'
  | 'LengthRequired'
  | 'Precondition'
  | 'PreconditionFailed'
  | 'PayloadTooLarge'
  | 'UriTooLong'
  | 'UnsupportedMediaType'
  | 'RangeNotSatisfiable'
  | 'ExpectationFailed'
  | 'ImATeapot'
  | 'Misdirected'
  | 'UnprocessableEntity'
  | 'FailedDependency'
  | 'RateLimit'
  | 'TooManyRequests'
  | 'Internal'
  | 'InternalServerError'
  | 'NotImplemented'
  | 'BadGateway'
  | 'Unavailable'
  | 'ServiceUnavailable'
  | 'Timeout'
  | 'GatewayTimeout'
  | 'HttpVersionNotSupported';

/** First-party behavior attached to a stable framework error code. */
export interface ErrorResponseOptions {
  readonly retryable: boolean;
}

/**
 * Full response metadata for an endpoint.
 * Carries both success responses and error declarations.
 */
export interface ResponseDeclarations {
  /** Success responses (2xx). At most one per status code. */
  readonly returns?: readonly ResponseMeta[];
  /** Framework-known error codes this endpoint may emit. */
  readonly errorCodes?: readonly ErrorResponseCode[];
  /** Explicit retryability keyed by the same stable code in `errorCodes`. */
  readonly errorOptions?: Readonly<Partial<Record<ErrorResponseCode, ErrorResponseOptions>>>;
  /**
   * Schema of the `details` member each code carries, declared with `.mayThrowDetails()`.
   * It describes `details` alone, never the `{code, error, message}` envelope.
   */
  readonly errorDetails?: Readonly<Partial<Record<ErrorResponseCode, SchemaDefinition>>>;
  /** Error responses (4xx/5xx). At most one per status code. */
  readonly throws?: readonly ResponseMeta[];
}

/**
 * Structured metadata captured from endpoint builder methods.
 * Used for OpenAPI documentation — does not affect runtime behavior.
 */
export interface EndpointMeta {
  /** Human-readable description for OpenAPI operation summary/description. */
  readonly description?: string;
  /** Security requirements from `.secure(options)`. Not set for guard functions. */
  readonly security?: SecurityOptions;
  /** True when .secure() uses an opaque guard that cannot be represented in a first-party contract. */
  readonly customSecurityGuard?: boolean;
  /** First-party generated-client policy declared beside this endpoint. */
  readonly client?: ClientOperationPolicy;
  /** Cache options from `.cache()`. */
  readonly cache?: HttpCacheOptions;
  /** CORS options from `.cors()`. */
  readonly cors?: CorsOptions;
  /** Rate-limit options from `.rateLimit()` (serializable subset). */
  readonly rateLimit?: { readonly windowMs?: number; readonly max?: number };
}
