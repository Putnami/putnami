import type { ClientCredentialProfile } from '@putnami/application';
import { ClientServiceConfigError } from './errors';
import type { Interceptor } from './transport.type';

/**
 * Static, non-secret request defaults of one service binding, keyed by
 * canonical header name. A snapshot is frozen and has no prototype, so a name
 * such as `__proto__` stays an ordinary own entry.
 */
export type BindingHeaders = Readonly<Record<string, string>>;

/**
 * Header names a binding can never supply, compared case-insensitively.
 *
 * Credential, identity, request-context, tracing, origin and transport headers
 * belong to the runtime or to `credentials`. The Go runtime reserves the same
 * set (go/framework/client/service_headers.go), and
 * protocols/clientcontract/fixtures/binding/headers.json pins both.
 */
export const RESERVED_BINDING_HEADERS: ReadonlySet<string> = new Set([
  'authorization',
  'cookie',
  'set-cookie',
  'host',
  'connection',
  'keep-alive',
  'upgrade',
  'content-type',
  'content-length',
  'content-encoding',
  'transfer-encoding',
  'te',
  'trailer',
  'accept',
  'accept-encoding',
  'traceparent',
  'tracestate',
  'baggage',
  'x-forwarded-for',
  'forwarded',
  'x-real-ip',
  'x-cloud-trace-context',
  'origin',
  'x-client-id',
  'x-request-id',
  'x-putnami-client-id',
  'x-putnami-service',
  'x-trace-id',
  'x-region',
  'x-experiments',
]);

/** Header name prefixes a binding can never supply, compared case-insensitively. */
export const RESERVED_BINDING_HEADER_PREFIXES: readonly string[] = Object.freeze([
  'sec-websocket-',
  'proxy-',
  'connect-',
  'grpc-',
]);

/** An RFC 9110 field name: one or more token characters. */
const TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

/**
 * Validate and snapshot a binding's static headers.
 *
 * Names are checked in code-point order, so the first refusal is the same on
 * every run and in both runtimes. Diagnostics never contain a value.
 */
export function snapshotBindingHeaders(source: unknown): BindingHeaders | undefined {
  if (source === undefined || source === null) return undefined;
  if (typeof source !== 'object' || Array.isArray(source)) {
    throw new ClientServiceConfigError('service binding headers must map names to values');
  }
  const entries = source as Record<string, unknown>;
  const headers: Record<string, string> = Object.create(null);
  const seen = new Set<string>();
  for (const name of Object.keys(entries).sort(byCodePoint)) {
    if (!TOKEN.test(name) || isReservedBindingHeader(name)) {
      throw new ClientServiceConfigError('service binding header name is invalid or reserved');
    }
    const lower = name.toLowerCase();
    if (seen.has(lower)) throw new ClientServiceConfigError('service binding headers contain duplicate names');
    seen.add(lower);
    const value = entries[name];
    if (typeof value !== 'string' || !isValidBindingHeaderValue(value)) {
      throw new ClientServiceConfigError(
        'service binding header value must be visible ASCII without surrounding whitespace',
      );
    }
    headers[canonicalHeaderName(name)] = value;
  }
  return Object.freeze(headers);
}

/**
 * Refuse a static header that names a provider-declared credential header.
 * Secrets belong in `credentials`, where the declared security policy selects
 * them per call.
 */
export function assertNoCredentialHeaderConflict(
  headers: BindingHeaders | undefined,
  profiles: Readonly<Record<string, ClientCredentialProfile>>,
): void {
  if (!headers) return;
  const names = new Set(Object.keys(headers).map((name) => name.toLowerCase()));
  for (const profile of Object.values(profiles)) {
    if ('header' in profile && names.has(profile.header.toLowerCase())) {
      throw new ClientServiceConfigError('service binding header conflicts with a declared credential header');
    }
  }
}

/**
 * Apply binding defaults to one call's private request, before the response
 * cache renders its key and before any attempt, credential or stream carrier
 * reads the headers.
 *
 * An explicit operation header wins, including an empty value. A static
 * idempotency key would reuse one key across distinct calls, so a binding that
 * names the operation's key header fails the call before dispatch.
 */
export function bindingHeadersInterceptor(headers: BindingHeaders): Interceptor {
  const entries = Object.entries(headers);
  return async (request, next) => {
    const keyHeader = request.clientOperation?.idempotency.keyHeader?.toLowerCase();
    if (keyHeader && entries.some(([name]) => name.toLowerCase() === keyHeader)) {
      throw new ClientServiceConfigError('service binding header conflicts with the operation idempotency key');
    }
    for (const [name, value] of entries) {
      if (!request.headers.has(name)) request.headers.set(name, value);
    }
    return next(request);
  };
}

function isReservedBindingHeader(name: string): boolean {
  const lower = name.toLowerCase();
  return (
    RESERVED_BINDING_HEADERS.has(lower) || RESERVED_BINDING_HEADER_PREFIXES.some((prefix) => lower.startsWith(prefix))
  );
}

/**
 * Visible ASCII with inner spaces or tabs. Fetch cannot carry other characters
 * as the bytes the Go runtime sends, and it trims surrounding whitespace that a
 * first-party WebSocket init frame would otherwise keep, so both runtimes
 * refuse them and every carrier sends the configured value unchanged.
 */
function isValidBindingHeaderValue(value: string): boolean {
  if (value !== '' && (isHeaderSpace(value.charCodeAt(0)) || isHeaderSpace(value.charCodeAt(value.length - 1)))) {
    return false;
  }
  for (let index = 0; index < value.length; index++) {
    const code = value.charCodeAt(index);
    if (code !== 0x09 && (code < 0x20 || code > 0x7e)) return false;
  }
  return true;
}

function isHeaderSpace(code: number): boolean {
  return code === 0x20 || code === 0x09;
}

/** The form Go's `http.CanonicalHeaderKey` gives a token name. */
function canonicalHeaderName(name: string): string {
  let upper = true;
  let canonical = '';
  for (const character of name) {
    canonical += upper ? character.toUpperCase() : character.toLowerCase();
    upper = character === '-';
  }
  return canonical;
}

function byCodePoint(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}
