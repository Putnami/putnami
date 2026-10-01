import type { ClientContractOperation, ClientSchema } from '@putnami/application';
import { decodeJsonBody, decodeJsonValue, encodeJsonBody } from './json-codec';

/** Maximum byte length for stored response bodies. Larger payloads are truncated. */
const MAX_RESPONSE_BODY_LENGTH = 4096;

/**
 * Truncate a response body to a safe size to avoid storing sensitive server internals
 * (SQL errors, stack traces, PII) that may be present in large error responses.
 */
function sanitizeResponseBody(body: unknown): unknown {
  if (body === undefined || body === null) return body;

  const text = typeof body === 'string' ? body : JSON.stringify(body);
  if (text === undefined) return undefined;
  if (text.length <= MAX_RESPONSE_BODY_LENGTH) return body;

  return `${text.slice(0, MAX_RESPONSE_BODY_LENGTH)}… [truncated]`;
}

/**
 * gRPC status carried by an error raised on a Connect/gRPC route.
 *
 * Optional on every error because HTTP-transport errors have no gRPC status;
 * {@link ConnectTransport} fills them in from the Connect error body (or the
 * mapped HTTP status) so callers can branch on `UNIMPLEMENTED` and friends.
 */
export interface ClientGrpcStatus {
  /** Numeric gRPC status code, e.g. `12` for `UNIMPLEMENTED`. */
  grpcCode?: number;
  /** Canonical gRPC status name, e.g. `'UNIMPLEMENTED'`. */
  grpcStatus?: string;
  /** Connect error details (`google.rpc.*` payloads) when the server sent any. */
  details?: readonly unknown[];
}

/**
 * Base error for all client errors.
 * Carries the service name, method, HTTP status, and original response.
 */
export class ClientError extends Error {
  readonly service: string;
  readonly method: string;
  readonly status: number;
  /** Response body from the server. Non-enumerable to prevent accidental serialization of sensitive data. */
  declare readonly responseBody: unknown;
  /** Numeric gRPC status when the response carried one (Connect/gRPC routes only). */
  readonly grpcCode?: number;
  /** Canonical gRPC status name when the response carried one. */
  readonly grpcStatus?: string;
  /** Connect error details when the response carried any. */
  readonly details?: unknown;

  constructor(
    options: {
      service: string;
      method: string;
      status: number;
      message: string;
      responseBody?: unknown;
      cause?: unknown;
    } & ClientGrpcStatus,
  ) {
    super(options.message, options.cause !== undefined ? { cause: options.cause } : undefined);
    this.name = 'ClientError';
    this.service = options.service;
    this.method = options.method;
    this.status = options.status;
    if (options.grpcCode !== undefined) this.grpcCode = options.grpcCode;
    if (options.grpcStatus !== undefined) this.grpcStatus = options.grpcStatus;
    if (options.details !== undefined) this.details = options.details;
    // Non-enumerable: won't appear in JSON.stringify or logger serializers,
    // preventing accidental leakage of server internals (SQL errors, stack traces, PII).
    // Body is truncated to limit exposure of sensitive data in large responses.
    if (options.responseBody !== undefined) {
      Object.defineProperty(this, 'responseBody', {
        value: sanitizeResponseBody(options.responseBody),
        enumerable: false,
        writable: false,
      });
    }
  }
}

/** Invalid deployment configuration for a generated first-party client. */
export class ClientServiceConfigError extends ClientError {
  readonly code = 'client.config';

  constructor(message: string) {
    super({ service: '', method: '', status: 0, message });
    this.name = 'ClientServiceConfigError';
  }
}

/** Credential acquisition or selection failed before a service attempt. */
export class ClientCredentialError extends ClientError {
  readonly code = 'client.credential';

  constructor(message = 'service credential acquisition failed') {
    super({ service: '', method: '', status: 0, message });
    this.name = 'ClientCredentialError';
  }
}

/** A caller value cannot be encoded under the generated request contract. */
export class ClientRequestEncodingError extends ClientError {
  readonly code = 'client.request';

  constructor(message: string) {
    super({ service: '', method: '', status: 0, message });
    this.name = 'ClientRequestEncodingError';
  }
}

/** A provider response does not satisfy the generated first-party contract. */
export class ClientResponseContractError extends ClientError {
  readonly code = 'client.response';

  constructor(message: string, service = '', method = '') {
    super({ service, method, status: 0, message });
    this.name = 'ClientResponseContractError';
  }
}

/** The caller canceled a generated operation. */
export class ClientCanceledError extends ClientError {
  readonly code = 'client.canceled';

  constructor(service = '', method = '') {
    super({ service, method, status: 0, message: 'service request was canceled' });
    this.name = 'ClientCanceledError';
  }
}

/** The generated operation exceeded its caller/provider deadline. */
export class ClientDeadlineError extends ClientError {
  readonly code = 'client.deadline';

  constructor(service = '', method = '') {
    super({ service, method, status: 0, message: 'service request deadline exceeded' });
    this.name = 'ClientDeadlineError';
  }
}

/**
 * A first-party error declared by the provider contract. `message` carries the
 * provider's own free-text envelope member only when the consuming binding set
 * `carryRemoteMessage` and the response had one (see `decodeFrameworkError`),
 * with every occurrence of this call's own credential material — raw,
 * standard base64 or raw URL base64 — replaced inline by `[REDACTED]` so the
 * surrounding prose survives, and a message that is one opaque base64 value
 * disclosing a secret replaced whole; otherwise it is a local, stable synthetic
 * message. `details` is exposed only when the provider declared a schema for
 * this code.
 */
export class ClientFrameworkError<
  TCode extends string = string,
  TDetails = unknown,
  TService extends string = string,
  TMethod extends string = string,
  TStatus extends number = number,
> extends ClientError {
  readonly code: TCode;
  declare readonly service: TService;
  declare readonly method: TMethod;
  declare readonly status: TStatus;
  declare readonly details?: TDetails;

  constructor(options: {
    service: TService;
    method: TMethod;
    status: TStatus;
    code: TCode;
    details?: TDetails;
    grpcCode?: number;
    /**
     * The provider's own free-text `message`, with this call's own credential
     * material already substituted inline by the caller (see
     * `decodeFrameworkError`), and passed only when the consuming binding
     * opted in. Absent or empty falls back to the local, stable synthetic
     * message — the default this class uses for every binding that did not
     * ask for provider prose.
     */
    message?: string;
  }) {
    super({
      service: options.service,
      method: options.method,
      status: options.status,
      message: options.message || `${options.service || 'service'} request failed with ${options.code}`,
      grpcCode: options.grpcCode,
    });
    this.name = 'ClientFrameworkError';
    this.code = options.code;
    if (options.details !== undefined) this.details = options.details;
  }
}

/** Match only a provider-declared error from one exact generated operation. */
export function isClientFrameworkError(
  error: unknown,
  identity: {
    service: string;
    method: string;
    status: number;
    code: string;
    detailsSchema?: ClientSchema;
    schemas?: Readonly<Record<string, ClientSchema>>;
  },
): error is ClientFrameworkError {
  if (
    !(error instanceof ClientFrameworkError) ||
    error.service !== identity.service ||
    error.method !== identity.method ||
    error.status !== identity.status ||
    error.code !== identity.code
  ) {
    return false;
  }
  if (!identity.detailsSchema) return error.details === undefined;
  if (error.details === undefined) return true;
  return projectTypedDetails(error.details, identity.detailsSchema, identity.schemas).valid;
}

/** Stable code and declared detail body carried by a first-party error response. */
export interface FirstPartyErrorEnvelope {
  /** `code` field: the stable error code, the only wire vocabulary. */
  readonly remoteCode?: string;
  /** `details` field: the body the operation declared for that code, if any. */
  readonly detailsPayload: unknown;
}

/**
 * Read the error envelope every first-party endpoint writes, in either language:
 * `{code, error, message, details?}`.
 *
 * The stable code and the declared detail body live in separate fields, so a
 * declared schema is validated against `details` alone and never against the
 * envelope carrying it. A body that is not an object carries no code; the caller
 * then has no declared identity to select and reports `client.remote` rather
 * than guessing one.
 */
export function readFirstPartyErrorEnvelope(payload: unknown): FirstPartyErrorEnvelope {
  if (!isRecord(payload)) return { detailsPayload: undefined };
  const code = readPayloadCode(payload);
  return { ...(code !== undefined ? { remoteCode: code } : {}), detailsPayload: payload['details'] };
}

/** Convert an untrusted first-party error payload into a local typed error. */
export function decodeFrameworkError(options: {
  service: string;
  method: string;
  status: number;
  payload: unknown;
  /** Stable code carried outside the declared detail body (SSE/WS envelopes). */
  remoteCode?: string;
  /** Declared detail body carried outside the transport envelope. */
  detailsPayload?: unknown;
  operation: ClientContractOperation;
  schemas?: Readonly<Record<string, ClientSchema>>;
  secrets?: readonly string[];
  /**
   * The consuming binding set `ServiceBinding.carryRemoteMessage`. Absent means
   * no, so a caller with no binding to read cannot carry provider prose by
   * omission.
   */
  carryRemoteMessage?: boolean;
}): ClientFrameworkError {
  const payloadCode = options.remoteCode ?? readPayloadCode(options.payload);
  const candidates = options.operation.errors.filter((entry) => entry.status === options.status);
  const declared = payloadCode
    ? candidates.find((entry) => entry.code === payloadCode)
    : candidates.length === 1
      ? candidates[0]
      : undefined;
  // Gated on the consumer, not on the declaration: a binding that opted in gets
  // the prose whether or not the code is declared, and one that did not gets
  // the synthetic message either way. WriteHTTPError/firstPartyErrorBody
  // already gate `message` to genuinely client-safe text server-side (a generic
  // placeholder for internal/infra/bug categories, the handler's own text
  // otherwise), so redacting this call's own secrets is enough to carry it
  // safely once the consumer asked for it (mirrors the Go runtime's
  // sanitizedErrorMessage, remote_error.go).
  const providerMessage = sanitizedErrorMessage(
    readPayloadMessage(options.payload),
    options.secrets ?? [],
    options.carryRemoteMessage === true,
  );
  if (!declared) {
    return new ClientFrameworkError({
      service: options.service,
      method: options.method,
      status: options.status,
      code: 'client.remote',
      ...(providerMessage ? { message: providerMessage } : {}),
    });
  }
  let details: unknown;
  if (declared.schema) {
    let decoded: unknown;
    try {
      const detailsPayload = Object.hasOwn(options, 'detailsPayload') ? options.detailsPayload : options.payload;
      decoded = decodeJsonValue(detailsPayload, declared.schema, options.schemas);
    } catch {
      throw new ClientResponseContractError(
        'provider error body does not match its declared schema',
        options.service,
        options.method,
      );
    }
    const redacted = redactSecrets(decoded, options.secrets ?? []);
    const projection = projectTypedDetails(redacted, declared.schema, options.schemas);
    if (projection.valid) {
      details = projection.value;
    } else {
      // Redaction can invalidate an enum, pattern, required key, or byte field.
      // Preserve the stable declared error identity and omit unsafe details.
      details = undefined;
    }
  }
  return new ClientFrameworkError({
    service: options.service,
    method: options.method,
    status: options.status,
    code: declared.code,
    ...(details !== undefined ? { details } : {}),
    ...(declared.grpcCode !== undefined ? { grpcCode: declared.grpcCode } : {}),
    ...(providerMessage ? { message: providerMessage } : {}),
  });
}

/**
 * Redact this call's own credential material from a provider's free-text
 * message, or drop it entirely when the consuming binding did not opt in.
 *
 * The message differs from `details` on purpose: free text has no declared
 * schema to break, so each occurrence of a secret — raw, in its standard base64
 * form or in its raw URL base64 form — is substituted inline and the prose
 * around it survives, which is what a human reading the error needs. A
 * structured scalar inside `details` has no such freedom and is dropped or
 * replaced whole to keep the declared shape (`redactSecrets`, unchanged). The
 * one whole-string case here is a message that is itself a base64 value whose
 * decoded bytes disclose a secret: an opaque blob has no position to
 * substitute at. The Go runtime applies the same two rules
 * (remote_error.go, sanitizedErrorMessage).
 */
function sanitizedErrorMessage(
  message: string | undefined,
  secrets: readonly string[],
  carryRemoteMessage: boolean,
): string | undefined {
  if (!carryRemoteMessage || !message) return undefined;
  const active = secrets.filter((secret) => secret.length > 0);
  if (active.length === 0) return message;
  const decoded = decodeBase64Payload(message);
  if (decoded !== undefined) {
    const text = new TextDecoder().decode(decoded);
    if (active.some((secret) => text.includes(secret))) return '[REDACTED]';
  }
  return active.reduce(
    (text, secret) =>
      [secret, standardBase64(secret), rawUrlBase64(secret)].reduce(
        (redacted, form) => redacted.split(form).join('[REDACTED]'),
        text,
      ),
    message,
  );
}

const BASE64_STANDARD = /^[A-Za-z0-9+/]+={0,2}$/;
const BASE64_URL = /^[A-Za-z0-9_-]+={0,2}$/;

/**
 * Decode text that is one base64 value, in the standard or the URL alphabet,
 * padded or not — the four decoders the Go runtime tries. Text that is not
 * base64 at all (prose has spaces) reports `undefined` so it is never compared
 * as if it were bytes.
 */
function decodeBase64Payload(text: string): Uint8Array | undefined {
  if (text.length < 4) return undefined;
  const alphabet = BASE64_STANDARD.test(text) ? 'base64' : BASE64_URL.test(text) ? 'base64url' : undefined;
  if (alphabet === undefined) return undefined;
  try {
    const decoded = Uint8Array.fromBase64(text, { alphabet });
    return decoded.length > 0 ? decoded : undefined;
  } catch {
    return undefined;
  }
}

/** The secret's UTF-8 bytes in padded standard base64 (Go `base64.StdEncoding`). */
function standardBase64(secret: string): string {
  return new TextEncoder().encode(secret).toBase64({ alphabet: 'base64' });
}

/** The secret's UTF-8 bytes in unpadded URL base64 (Go `base64.RawURLEncoding`). */
function rawUrlBase64(secret: string): string {
  return new TextEncoder().encode(secret).toBase64({ alphabet: 'base64url', omitPadding: true });
}

function readPayloadMessage(payload: unknown): string | undefined {
  if (!isRecord(payload)) return undefined;
  return typeof payload['message'] === 'string' && payload['message'] ? payload['message'] : undefined;
}

function readPayloadCode(payload: unknown): string | undefined {
  if (!isRecord(payload)) return undefined;
  return typeof payload['code'] === 'string' && payload['code'] ? payload['code'] : undefined;
}

function redactSecrets(value: unknown, secrets: readonly string[]): unknown {
  const active = secrets.filter((secret) => secret.length > 0);
  if (typeof value === 'string') {
    return active.reduce((text, secret) => text.split(secret).join('[REDACTED]'), value);
  }
  if (typeof value === 'number' || typeof value === 'bigint' || typeof value === 'boolean') {
    const text = String(value);
    return active.some((secret) => text.includes(secret)) ? '[REDACTED]' : value;
  }
  if (value instanceof Uint8Array) {
    const text = new TextDecoder().decode(value);
    return active.some((secret) => text.includes(secret)) ? '[REDACTED]' : value.slice();
  }
  if (Array.isArray(value)) return value.map((item) => redactSecrets(item, active));
  if (isRecord(value)) {
    return Object.fromEntries(
      Object.entries(value).map(([key, child]) => [redactSecrets(key, active), redactSecrets(child, active)]),
    );
  }
  return value;
}

function projectTypedDetails(
  value: unknown,
  schema: ClientSchema,
  schemas: Readonly<Record<string, ClientSchema>> = {},
): { valid: true; value: unknown } | { valid: false } {
  try {
    // The schema codec validates the TypeScript representation before writing,
    // then recreates bigint and Uint8Array from the exact JSON wire. Comparing
    // both sides also rejects properties that a projection would otherwise omit.
    const projected = decodeJsonBody(encodeJsonBody(value, schema, schemas), schema, schemas);
    return sameTypedValue(value, projected) ? { valid: true, value: projected } : { valid: false };
  } catch {
    return { valid: false };
  }
}

function sameTypedValue(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true;
  if (left instanceof Uint8Array || right instanceof Uint8Array) {
    return (
      left instanceof Uint8Array &&
      right instanceof Uint8Array &&
      left.length === right.length &&
      left.every((value, index) => value === right[index])
    );
  }
  if (Array.isArray(left) || Array.isArray(right)) {
    return (
      Array.isArray(left) &&
      Array.isArray(right) &&
      left.length === right.length &&
      left.every((value, index) => sameTypedValue(value, right[index]))
    );
  }
  if (!isRecord(left) || !isRecord(right)) return false;
  const leftKeys = Object.keys(left).sort();
  const rightKeys = Object.keys(right).sort();
  return (
    leftKeys.length === rightKeys.length &&
    leftKeys.every((key, index) => key === rightKeys[index] && sameTypedValue(left[key], right[key]))
  );
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Thrown when the server returns a 4xx error.
 */
export class ClientRequestError extends ClientError {
  constructor(
    options: {
      service: string;
      method: string;
      status: number;
      message: string;
      responseBody?: unknown;
    } & ClientGrpcStatus,
  ) {
    super(options);
    this.name = 'ClientRequestError';
  }
}

/**
 * Thrown when the server returns a 5xx error or the request fails entirely.
 */
export class ClientServerError extends ClientError {
  constructor(
    options: {
      service: string;
      method: string;
      status: number;
      message: string;
      responseBody?: unknown;
    } & ClientGrpcStatus,
  ) {
    super(options);
    this.name = 'ClientServerError';
  }
}

/**
 * Delivered to a stream's `onError` when a Connect stream ends with a non-OK
 * gRPC status.
 *
 * The HTTP response itself succeeded (Connect streams always answer `200` and
 * carry the outcome in the end-of-stream trailers), so `status` is `0` and the
 * real outcome lives in `grpcCode`/`grpcStatus`.
 */
export class ClientStreamError extends ClientError {
  constructor(
    options: {
      service: string;
      method: string;
      message: string;
      responseBody?: unknown;
      /**
       * The HTTP status the failure carried, when it had one. A stream refused
       * before its first message fails at the HTTP layer, and the consumer
       * needs that status to tell an unauthenticated call from a forbidden one
       * — the Connect protocol's own status-to-code inference reads it. A
       * failure stated in the end-of-stream message has none: it is `0`.
       */
      status?: number;
    } & ClientGrpcStatus,
  ) {
    super({ ...options, status: options.status ?? 0 });
    this.name = 'ClientStreamError';
  }
}

/**
 * Thrown (or delivered to a stream's `onError`) when the transport a call needs
 * does not exist in this runtime.
 *
 * The concrete case: a client/bidi RPC answered `UNIMPLEMENTED` on the Connect
 * route has to fall back to the WebSocket transport, and the host has no
 * `WebSocket` global. Surfacing this typed error keeps the failure legible
 * instead of a bare `ReferenceError` (or a raw 501) escaping the transport.
 */
export class ClientTransportUnavailableError extends ClientError {
  /** The transport that was required but unavailable, e.g. `'websocket'`. */
  readonly transport: string;

  constructor(
    options: {
      service: string;
      method: string;
      transport: string;
      message?: string;
    } & ClientGrpcStatus,
  ) {
    super({
      ...options,
      status: 0,
      message:
        options.message ??
        `No ${options.transport} transport available in this runtime for ${options.method || 'the requested stream'}`,
    });
    this.name = 'ClientTransportUnavailableError';
    this.transport = options.transport;
  }
}

/**
 * Thrown when a request exceeds the configured `timeoutMs`.
 * Carries the timeout value that was exceeded; `status` is `0` (no HTTP response).
 */
export class ClientTimeoutError extends ClientError {
  /** The timeout value (ms) that was exceeded. */
  readonly timeoutMs: number;

  constructor(options: { service: string; method: string; timeoutMs: number; message?: string; cause?: unknown }) {
    super({
      service: options.service,
      method: options.method,
      status: 0,
      message: options.message ?? `Request timed out after ${options.timeoutMs}ms`,
      cause: options.cause,
    });
    this.name = 'ClientTimeoutError';
    this.timeoutMs = options.timeoutMs;
  }
}

/**
 * Thrown when a response body exceeds the transport's configured maximum size.
 *
 * Mirrors the Go transports' `CodeClientResponse` "response body exceeds maximum
 * size" failure: the read is capped (Go reads one byte past the limit to detect
 * overflow) and an oversized body fails the request instead of being silently
 * truncated. `status` is `0` because the body was never fully materialized.
 */
export class ClientResponseSizeError extends ClientError {
  readonly code = 'client.response';
  /** The maximum response body size (bytes) that was exceeded. */
  readonly maxResponseSize: number;

  constructor(options: { service?: string; method?: string; maxResponseSize: number; message?: string }) {
    super({
      service: options.service ?? '',
      method: options.method ?? '',
      status: 0,
      message: options.message ?? `Response body exceeds maximum size of ${options.maxResponseSize} bytes`,
    });
    this.name = 'ClientResponseSizeError';
    this.maxResponseSize = options.maxResponseSize;
  }
}

/**
 * Thrown when all retry attempts have been exhausted on a transient (network) failure.
 * Carries the total number of attempts made and the last underlying error.
 */
export class ClientRetryExhaustedError extends ClientError {
  /** Total number of attempts made (initial attempt + retries). */
  readonly attempts: number;
  /** The last error that caused the retry to give up. */
  readonly lastError: Error;

  constructor(options: { service: string; method: string; attempts: number; lastError: Error; message?: string }) {
    super({
      service: options.service,
      method: options.method,
      status: 0,
      message: options.message ?? `Retry exhausted after ${options.attempts} attempts: ${options.lastError.message}`,
      cause: options.lastError,
    });
    this.name = 'ClientRetryExhaustedError';
    this.attempts = options.attempts;
    this.lastError = options.lastError;
  }
}
