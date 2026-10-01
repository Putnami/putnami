import type { ErrorResponseCode } from './response-meta';

/**
 * Canonical wire code for each framework error code, mirroring the Go framework's
 * `go/framework/errors` (`code.go`, `http.go`): dotted, snake_case identifiers such as
 * `not_found` or `http.bad_request`. `ErrorResponseCode` (e.g. `'NotFound'`) stays the
 * PascalCase `.mayThrow()` DX affordance; this table is what actually appears in
 * `x-putnami-client`, the client IR, and the HTTP error envelope on the wire.
 *
 * A TS code without a Go framework equivalent maps to `'unknown'`, mirroring Go's own
 * `errors.FromStatus` fallback for a status it does not register a canonical code for.
 */
export const errorCodeToStableCode: Record<ErrorResponseCode, string> = {
  BadRequest: 'http.bad_request',
  Validation: 'validation',
  InvalidArgument: 'invalid_argument',
  Unauthorized: 'unauthorized',
  Forbidden: 'forbidden',
  NotFound: 'not_found',
  MethodNotAllowed: 'http.method_not_allowed',
  NotAcceptable: 'unknown',
  ProxyAuthenticationRequired: 'unknown',
  RequestTimeout: 'unknown',
  Conflict: 'conflict',
  AlreadyExists: 'already_exists',
  Gone: 'unknown',
  LengthRequired: 'unknown',
  Precondition: 'precondition_failed',
  PreconditionFailed: 'precondition_failed',
  PayloadTooLarge: 'http.payload_too_large',
  UriTooLong: 'unknown',
  UnsupportedMediaType: 'http.unsupported_media_type',
  RangeNotSatisfiable: 'unknown',
  ExpectationFailed: 'unknown',
  ImATeapot: 'unknown',
  Misdirected: 'unknown',
  UnprocessableEntity: 'http.unprocessable_entity',
  FailedDependency: 'unknown',
  RateLimit: 'http.too_many_requests',
  TooManyRequests: 'http.too_many_requests',
  Internal: 'internal',
  InternalServerError: 'http.internal_server',
  NotImplemented: 'not_implemented',
  BadGateway: 'http.bad_gateway',
  Unavailable: 'http.service_unavailable',
  ServiceUnavailable: 'http.service_unavailable',
  Timeout: 'http.gateway_timeout',
  GatewayTimeout: 'http.gateway_timeout',
  HttpVersionNotSupported: 'unknown',
};

/** The implicit pair every first-party operation carries, aligned on Go's `statusToCode`. */
export const IMPLICIT_BAD_REQUEST_CODE = 'http.bad_request';
export const IMPLICIT_INTERNAL_CODE = 'http.internal_server';

/**
 * The two refusals a declared raw octet body adds. They are declared, not
 * implicit: an endpoint that bounds its payload must publish what it answers
 * when the bound or the media type is violated, otherwise a generated client
 * reads its own refusal as an unattributed remote failure.
 */
export const PAYLOAD_TOO_LARGE_CODE = 'http.payload_too_large';
export const UNSUPPORTED_MEDIA_TYPE_CODE = 'http.unsupported_media_type';

/**
 * Best-effort stable wire code for a runtime `HttpException`. Strips the `Exception`
 * suffix a framework subclass name carries (`NotFoundException` → `NotFound`) to recover
 * the `ErrorResponseCode` DX identifier, then resolves it through `errorCodeToStableCode`.
 * Falls back to the implicit pair by status, then to `'unknown'`, for an exception the
 * endpoint never declared with `.mayThrow()`/`.mayThrowWith()`.
 */
export function stableWireCode(exceptionCode: string, status: number): string {
  const stripped = exceptionCode.endsWith('Exception') ? exceptionCode.slice(0, -'Exception'.length) : exceptionCode;
  const known = (errorCodeToStableCode as Record<string, string>)[stripped];
  if (known) return known;
  if (status === 400) return IMPLICIT_BAD_REQUEST_CODE;
  if (status === 500) return IMPLICIT_INTERNAL_CODE;
  return 'unknown';
}
