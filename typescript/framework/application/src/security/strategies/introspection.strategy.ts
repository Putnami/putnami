import { createHash } from 'node:crypto';
import { useLogger } from '@putnami/runtime';
import { type BreakerConfig, CircuitBreaker } from '../introspect-breaker';
import { PrincipalKind } from '../identity.constants';
import type { Principal } from '../security.types';
import { type Claims, claimMatchesAudience } from '../security.utils';
import { type AuthStrategy, extractBearerToken } from './auth-strategy';

// --- Constants (Go parity) ---

/**
 * Hard cap on the introspection response body, enforced BEFORE the body is
 * buffered or parsed so a hostile/broken endpoint cannot exhaust memory. Byte-for
 * byte the same 64 KiB as `maxIntrospectionSize` in
 * `go/framework/security/introspect.go`.
 */
export const MAX_INTROSPECTION_BYTES = 64 << 10;

/** 1 MiB cap on the OIDC discovery document (Go's `maxDiscoverySize`). */
const MAX_DISCOVERY_BYTES = 1 << 20;

const DEFAULT_INTROSPECT_TIMEOUT_MS = 5000;
const DEFAULT_INTROSPECT_CACHE_TTL_MS = 5 * 60 * 1000;
const INTROSPECT_CACHE_PREFIX = 'introspect:';

/**
 * Caps the wait before the single retry of a 429 Too Many Requests, whatever
 * `Retry-After` asks for, so backpressure costs a request at most this much extra
 * latency before it fails closed. Go's `maxIntrospectRetryDelay`.
 */
export const MAX_INTROSPECT_RETRY_DELAY_MS = 1000;

/**
 * When the 429 carries no usable `Retry-After`, the delay before that retry is
 * drawn from [MIN, MAX). Go's `minIntrospectRetryJitter` and
 * `maxIntrospectRetryJitter`.
 */
export const MIN_INTROSPECT_RETRY_JITTER_MS = 50;
export const MAX_INTROSPECT_RETRY_JITTER_MS = 250;

/**
 * RFC 7523 §2.2 `client_assertion_type` announcing that the accompanying
 * `client_assertion` is a signed JWT proving this resource server's identity.
 */
const CLIENT_ASSERTION_TYPE_JWT_BEARER = 'urn:ietf:params:oauth:client-assertion-type:jwt-bearer';

/** How this resource server authenticates itself to the introspection endpoint. */
export type ClientAuthMethod = 'basic' | 'post';

/**
 * A cache of active introspection results. Keyed by a sha256 digest that never
 * contains the raw token or client secret in the clear. Implementations must be
 * in-process or otherwise safe to share; see {@link MemoryIntrospectionCache}.
 */
export interface IntrospectionCache {
  /** Returns the cached introspection payload, or `undefined` on a miss/expiry. */
  get(key: string): Record<string, unknown> | undefined;
  /** Stores an active introspection payload for at most `ttlMs`. */
  set(key: string, payload: Record<string, unknown>, ttlMs: number): void;
}

/** Configures the {@link introspectionStrategy}. */
export interface IntrospectionStrategyOptions {
  /**
   * RFC 7662 introspection endpoint URL. When absent and {@link issuer} is set it
   * is discovered from the issuer's OIDC/OAuth metadata (`introspection_endpoint`).
   */
  endpoint?: string;
  /** OIDC/OAuth issuer URL used to discover {@link endpoint}. Ignored when endpoint is set. */
  issuer?: string;
  /** Client id authenticating THIS resource server to the endpoint (RFC 7662 §2.1). */
  clientId?: string;
  /** Client secret authenticating THIS resource server. NEVER logged. */
  clientSecret?: string;
  /** How the client credentials are transmitted. Defaults to `basic` (HTTP Basic). */
  clientAuth?: ClientAuthMethod;
  /**
   * RFC 7523 client-assertion resolver. When set it takes precedence over
   * clientId/clientSecret: the returned signed JWT is sent as
   * `client_assertion`/`client_assertion_type` and the shared secret is not
   * transmitted at all. A resolver error or empty assertion fails the request
   * closed (no fallback to the shared secret). The resolved assertion is NEVER
   * logged.
   */
  clientAssertion?: () => string | undefined | Promise<string | undefined>;
  /** Optional RFC 7662 §2.1 `token_type_hint`. */
  tokenTypeHint?: string;
  /** When set, an active token whose `aud` does not include this is rejected. */
  audience?: string | string[];
  /** Backs active results. Omit to hit the endpoint on every request. */
  cache?: IntrospectionCache;
  /** Bounds how long an active result is cached (capped by the token's `exp`). Default 5m. */
  cacheTtlMs?: number;
  /** Bounds each introspection HTTP request. Default 5s. */
  timeoutMs?: number;
  /**
   * Bounds the client-assertion resolver. Defaults to {@link timeoutMs}. It is
   * also the budget for a 429 retry, measured from the start of the upstream
   * call: the retry is skipped when the `Retry-After` wait cannot fit in what is
   * left, and the retry's resolver call and request are bounded by what is left.
   */
  clientAssertionTimeoutMs?: number;
  /** Permits a plaintext http endpoint on a non-loopback host. Do not enable in production. */
  allowInsecure?: boolean;
  /**
   * Optional in-process circuit breaker guarding the upstream call during an
   * outage. A 429 Too Many Requests is backpressure from a live endpoint, not an
   * outage: a 429 that persists after the single retry fails only that request
   * closed, never counts toward `failureThreshold`, never resets the failure run,
   * and frees a half-open probe slot for the next caller.
   */
  breaker?: BreakerConfig;
}

/**
 * A minimal in-process introspection cache with per-entry TTL. External caches
 * (Redis/Postgres) can implement {@link IntrospectionCache} instead; this default
 * keeps the fast path dependency-free.
 */
export class MemoryIntrospectionCache implements IntrospectionCache {
  private readonly store = new Map<string, { payload: Record<string, unknown>; expiresAt: number }>();

  get(key: string): Record<string, unknown> | undefined {
    const entry = this.store.get(key);
    if (!entry) {
      return undefined;
    }
    if (entry.expiresAt <= Date.now()) {
      this.store.delete(key);
      return undefined;
    }
    return entry.payload;
  }

  set(key: string, payload: Record<string, unknown>, ttlMs: number): void {
    this.store.set(key, { payload, expiresAt: Date.now() + ttlMs });
  }
}

/**
 * Derives the cache key from the token and the configuration that defines its
 * validation authority. The raw token and client secret are never used directly
 * as the key, so they never sit in a cache map or on disk in the clear. Byte-for
 * byte identical to `introspectCacheKey` in `go/framework/security/introspect.go`:
 * sha256 over the NUL-joined fields, hex-encoded, under the `introspect:` prefix.
 */
export function introspectCacheKey(
  endpoint: string,
  cfg: { issuer?: string; clientId?: string; clientSecret?: string },
  token: string,
): string {
  const NUL = String.fromCharCode(0);
  const joined = [endpoint, cfg.issuer ?? '', cfg.clientId ?? '', cfg.clientSecret ?? '', token].join(NUL);
  return INTROSPECT_CACHE_PREFIX + createHash('sha256').update(joined, 'utf8').digest('hex');
}

// --- Retry-After (Go parity: introspectRetryDelay / net/http ParseTime) ---

const SHORT_DAY_NAMES = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'];
const LONG_DAY_NAMES = ['sunday', 'monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday'];
const MONTH_NAMES = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'];
const DAYS_IN_MONTH = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];

// The three RFC 9110 §5.6.7 HTTP-date forms, with the field widths Go's
// http.ParseTime layouts accept: IMF-fixdate ("Sun, 06 Nov 1994 08:49:37 GMT"),
// the obsolete RFC 850 form ("Sunday, 06-Nov-94 08:49:37 GMT"), and the
// obsolete asctime form ("Sun Nov  6 08:49:37 1994", which carries no zone and
// is UTC).
const IMF_FIXDATE = /^([A-Za-z]{3}), (\d{2}) ([A-Za-z]{3}) (\d{4}) (\d{1,2}):(\d{2}):(\d{2}) GMT$/;
const RFC850_DATE = /^([A-Za-z]+), (\d{2})-([A-Za-z]{3})-(\d{2}) (\d{1,2}):(\d{2}):(\d{2}) (?:GMT|UTC)$/;
const ASCTIME_DATE = /^([A-Za-z]{3}) ([A-Za-z]{3}) {1,2}(\d{1,2}) (\d{1,2}):(\d{2}):(\d{2}) (\d{4})$/;

/** The fields of one HTTP-date form, before validation. */
interface HttpDateFields {
  weekday: string;
  dayNames: readonly string[];
  day: string;
  month: string;
  year: number;
  hour: string;
  minute: string;
  second: string;
}

function matchHttpDate(value: string): HttpDateFields | undefined {
  const imf = IMF_FIXDATE.exec(value);
  if (imf) {
    const [, weekday, day, month, year, hour, minute, second] = imf;
    return { weekday, dayNames: SHORT_DAY_NAMES, day, month, year: Number(year), hour, minute, second };
  }
  const rfc850 = RFC850_DATE.exec(value);
  if (rfc850) {
    const [, weekday, day, month, yy, hour, minute, second] = rfc850;
    const twoDigitYear = Number(yy);
    const year = twoDigitYear >= 69 ? 1900 + twoDigitYear : 2000 + twoDigitYear;
    return { weekday, dayNames: LONG_DAY_NAMES, day, month, year, hour, minute, second };
  }
  const asctime = ASCTIME_DATE.exec(value);
  if (asctime) {
    const [, weekday, month, day, hour, minute, second, year] = asctime;
    return { weekday, dayNames: SHORT_DAY_NAMES, day, month, year: Number(year), hour, minute, second };
  }
  return undefined;
}

/**
 * Parses an HTTP-date into epoch milliseconds, or returns `undefined`. It is
 * explicit rather than `Date.parse`, whose handling of the two obsolete forms
 * is engine-specific (asctime would be read as local time). Day and month names
 * match case-insensitively and the weekday is not checked against the date, as
 * in Go; a two-digit RFC 850 year maps 69-99 to 19xx and 00-68 to 20xx, as in
 * Go. The RFC 850 form accepts only the GMT and UTC zones.
 */
function parseHttpDate(value: string): number | undefined {
  const fields = matchHttpDate(value);
  if (!fields) {
    return undefined;
  }
  const monthIndex = MONTH_NAMES.indexOf(fields.month.toLowerCase());
  if (monthIndex < 0 || !fields.dayNames.includes(fields.weekday.toLowerCase())) {
    return undefined;
  }
  const { year } = fields;
  const day = Number(fields.day);
  const hour = Number(fields.hour);
  const minute = Number(fields.minute);
  const second = Number(fields.second);
  const leap = (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0;
  const daysInMonth = monthIndex === 1 && leap ? 29 : DAYS_IN_MONTH[monthIndex];
  if (day < 1 || day > daysInMonth || hour > 23 || minute > 59 || second > 59) {
    return undefined;
  }
  // setUTCFullYear, unlike Date.UTC, does not map years 0-99 to 19xx.
  const at = new Date(0);
  at.setUTCFullYear(year, monthIndex, day);
  at.setUTCHours(hour, minute, second, 0);
  return at.getTime();
}

/**
 * Returns how long a throttled introspection waits, in milliseconds, before its
 * single retry, from the 429's `Retry-After` header (RFC 9110 §10.2.3): either
 * delta-seconds or an HTTP-date resolved against `now` (epoch milliseconds).
 * Every result is clamped to [0, {@link MAX_INTROSPECT_RETRY_DELAY_MS}], so an
 * oversized value cannot hold a request for long and a past date retries at
 * once. An absent or malformed header (a sign, a fraction, garbage) yields a
 * short jittered delay instead, so resource servers throttled at the same
 * instant do not retry in lockstep. Case for case the same as
 * `introspectRetryDelay` in `go/framework/security/introspect.go`.
 */
export function introspectRetryDelayMs(retryAfter: string | null | undefined, now: number): number {
  const value = (retryAfter ?? '').trim();
  // delta-seconds is 1*DIGIT: no sign, no fraction.
  if (/^\d+$/.test(value)) {
    const delayMs = Number(value) * 1000; // a value past float range is Infinity, so it caps too
    return delayMs > MAX_INTROSPECT_RETRY_DELAY_MS ? MAX_INTROSPECT_RETRY_DELAY_MS : delayMs;
  }
  const at = parseHttpDate(value);
  if (at !== undefined) {
    return Math.min(Math.max(at - now, 0), MAX_INTROSPECT_RETRY_DELAY_MS);
  }
  // Non-cryptographic jitter: it only spreads retries, it guards no secret.
  return (
    MIN_INTROSPECT_RETRY_JITTER_MS +
    Math.floor(Math.random() * (MAX_INTROSPECT_RETRY_JITTER_MS - MIN_INTROSPECT_RETRY_JITTER_MS))
  );
}

// --- URL scheme policy (Go parity: requireSecureURL / isLoopbackHost) ---

function isLoopbackHost(host: string): boolean {
  if (host === 'localhost') {
    return true;
  }
  if (host === '::1' || host === '[::1]') {
    return true;
  }
  return /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host);
}

/**
 * Reports whether a URL may be contacted: https always, http only for loopback
 * hosts (local development) or when `allowInsecure` is set. Mirrors Go's
 * `requireSecureURL` so a network attacker cannot substitute a plaintext endpoint.
 */
function isSecureUrl(rawUrl: string, allowInsecure: boolean): boolean {
  let u: URL;
  try {
    u = new URL(rawUrl);
  } catch {
    return false;
  }
  if (u.protocol === 'https:') {
    return true;
  }
  if (u.protocol === 'http:') {
    return allowInsecure || isLoopbackHost(u.hostname);
  }
  return false;
}

// --- Response body helpers ---

/**
 * Reads at most `maxBytes` from a response body and decodes it as UTF-8. The cap
 * is applied while streaming — the full body is never buffered — mirroring Go's
 * `io.LimitReader(resp.Body, maxIntrospectionSize)`.
 */
async function readCapped(res: Response, maxBytes: number): Promise<string> {
  if (!res.body) {
    return '';
  }
  const reader = res.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (total < maxBytes) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      if (value && value.length > 0) {
        const remaining = maxBytes - total;
        const slice = value.length > remaining ? value.subarray(0, remaining) : value;
        chunks.push(slice);
        total += slice.length;
      }
    }
  } finally {
    // Release the connection; we deliberately stop reading past the cap.
    await reader.cancel().catch(() => {});
  }
  return Buffer.concat(chunks).toString('utf8');
}

function parseIntrospection(raw: string): { payload?: Record<string, unknown>; active: boolean } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { active: false };
  }
  if (!parsed || typeof parsed !== 'object') {
    return { active: false };
  }
  const payload = parsed as Record<string, unknown>;
  return { payload, active: payload['active'] === true };
}

/**
 * Maps an active introspection payload to a Principal, dropping the RFC 7662
 * `active` envelope flag (it gates authentication; it is not an identity claim).
 * Returns a fresh object so cached payloads are never shared or mutated across
 * requests.
 */
function principalFromPayload(payload: Record<string, unknown>): Principal {
  const rest: Record<string, unknown> = { ...payload };
  rest['active'] = undefined;
  // An RFC 7662 introspected principal is a `user` in the PrincipalKind
  // vocabulary unless the introspection response declares its own `kind`. The
  // default makes `.secure({ principalKind: 'user' })` satisfiable via the
  // introspection strategy, matching bearerJwtStrategy and apiKeyStrategy.
  if (typeof rest['kind'] !== 'string') {
    rest['kind'] = PrincipalKind.User;
  }
  return rest as Principal;
}

/**
 * Caps the configured TTL by the token's own `exp` so a cached result never
 * outlives the token. Returns 0 (do not cache) once `exp` has passed.
 */
function introspectCacheTtlMs(payload: Record<string, unknown>, maxTtlMs: number): number {
  const exp = payload['exp'];
  if (typeof exp !== 'number') {
    return maxTtlMs;
  }
  const untilMs = exp * 1000 - Date.now();
  if (untilMs <= 0) {
    return 0;
  }
  return Math.min(untilMs, maxTtlMs);
}

function withTimeout<T>(factory: () => Promise<T>, ms: number, label: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`${label} timed out`)), ms);
    factory().then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err) => {
        clearTimeout(timer);
        reject(err);
      },
    );
  });
}

/** The outcome of one upstream introspection call, shared by singleflight waiters. */
interface UpstreamResult {
  payload?: Record<string, unknown>;
  active: boolean;
  /** HTTP status, or 0 on a transport / client-assertion error. */
  status: number;
  error?: unknown;
}

async function discoverIntrospectionUrl(
  issuer: string,
  allowInsecure: boolean,
  timeoutMs: number,
): Promise<string | undefined> {
  const discoveryUrl = `${issuer}/.well-known/openid-configuration`;
  if (!isSecureUrl(discoveryUrl, allowInsecure)) {
    return undefined;
  }
  try {
    const res = await fetch(discoveryUrl, {
      headers: { Accept: 'application/json' },
      signal: AbortSignal.timeout(timeoutMs),
      redirect: 'error',
    });
    if (res.status !== 200) {
      return undefined;
    }
    const parsed = parseIntrospection(await readCapped(res, MAX_DISCOVERY_BYTES)).payload;
    const endpoint = parsed?.['introspection_endpoint'];
    if (typeof endpoint !== 'string' || !isSecureUrl(endpoint, allowInsecure)) {
      return undefined;
    }
    return endpoint;
  } catch {
    return undefined;
  }
}

/**
 * Authenticates an opaque bearer token via RFC 7662 OAuth2 Token Introspection.
 * TypeScript twin of `go/framework/security/introspect.go`.
 *
 * On each request it extracts the bearer token, consults the cache (if any),
 * otherwise POSTs the token to the introspection endpoint authenticated with the
 * client credentials (or an RFC 7523 client assertion). An active response maps
 * to a Principal and is cached; an inactive response resolves nothing and is
 * never cached. Transport errors, timeouts, and non-OK responses fail closed.
 * A 429 Too Many Requests is backpressure, not an outage: the upstream call is
 * retried once after the endpoint's `Retry-After` (delta-seconds or HTTP-date,
 * capped at 1s; a short jittered delay when absent), within
 * `clientAssertionTimeoutMs`. A 429 that persists fails that request closed and
 * never trips the circuit breaker. Concurrent requests for the same
 * not-yet-cached token collapse into a single upstream call, including that
 * retry; an optional in-process circuit breaker short-circuits uncached tokens
 * during a sustained endpoint outage.
 *
 * Security invariants: the response body is capped at {@link MAX_INTROSPECTION_BYTES}
 * before parsing; the client secret and client assertion are NEVER logged; the
 * endpoint must be https unless it is loopback or `allowInsecure` is set.
 */
export const introspectionStrategy = (options: IntrospectionStrategyOptions): AuthStrategy => {
  const clientAuth: ClientAuthMethod = options.clientAuth ?? 'basic';
  const timeoutMs = options.timeoutMs && options.timeoutMs > 0 ? options.timeoutMs : DEFAULT_INTROSPECT_TIMEOUT_MS;
  const cacheTtlMs =
    options.cacheTtlMs && options.cacheTtlMs > 0 ? options.cacheTtlMs : DEFAULT_INTROSPECT_CACHE_TTL_MS;
  const assertionTimeoutMs =
    options.clientAssertionTimeoutMs && options.clientAssertionTimeoutMs > 0
      ? options.clientAssertionTimeoutMs
      : timeoutMs;
  const allowInsecure = options.allowInsecure ?? false;
  const cache = options.cache;
  const breaker = CircuitBreaker.create(options.breaker);

  // Resolved endpoint is memoized on success and left retryable on failure so a
  // transient discovery outage does not disable the resolver until restart.
  let endpoint: string | undefined;
  let discoveryInflight: Promise<string | undefined> | undefined;
  const inflight = new Map<string, Promise<UpstreamResult>>();

  const resolveEndpoint = async (): Promise<string | undefined> => {
    if (endpoint) {
      return endpoint;
    }
    if (options.endpoint) {
      if (!isSecureUrl(options.endpoint, allowInsecure)) {
        return undefined;
      }
      endpoint = options.endpoint;
      return endpoint;
    }
    if (!options.issuer) {
      return undefined;
    }
    if (!discoveryInflight) {
      discoveryInflight = discoverIntrospectionUrl(options.issuer, allowInsecure, timeoutMs);
    }
    const discovered = await discoveryInflight;
    if (discovered) {
      endpoint = discovered;
    } else {
      discoveryInflight = undefined; // allow a later request to retry discovery
    }
    return endpoint;
  };

  /**
   * Bounds one step of an upstream call by its own timeout and, on a retry, by
   * the leader budget left before `deadline` (epoch milliseconds).
   */
  const stepTimeoutMs = (ms: number, deadline: number | undefined): number =>
    deadline === undefined ? ms : Math.max(0, Math.min(ms, deadline - Date.now()));

  const introspectToken = async (
    ep: string,
    token: string,
    deadline?: number,
  ): Promise<{ body: string; status: number; retryAfter: string | null }> => {
    const form = new URLSearchParams();
    form.set('token', token);
    if (options.tokenTypeHint) {
      form.set('token_type_hint', options.tokenTypeHint);
    }

    const headers: Record<string, string> = {
      'Content-Type': 'application/x-www-form-urlencoded',
      Accept: 'application/json',
    };

    if (options.clientAssertion) {
      // RFC 7523: the assertion always travels as body parameters and takes
      // precedence over the shared secret, which is not transmitted at all.
      const assertion = await withTimeout(
        async () => options.clientAssertion?.(),
        stepTimeoutMs(assertionTimeoutMs, deadline),
        'client assertion',
      );
      if (!assertion) {
        throw new Error('client assertion resolver returned an empty assertion');
      }
      form.set('client_assertion_type', CLIENT_ASSERTION_TYPE_JWT_BEARER);
      form.set('client_assertion', assertion);
    } else if (clientAuth === 'post') {
      if (options.clientId) {
        form.set('client_id', options.clientId);
      }
      if (options.clientSecret) {
        form.set('client_secret', options.clientSecret);
      }
    } else if (options.clientId || options.clientSecret) {
      // client_secret_basic (default): credentials travel in the header, not the body.
      const cred = Buffer.from(`${options.clientId ?? ''}:${options.clientSecret ?? ''}`).toString('base64');
      headers['Authorization'] = `Basic ${cred}`;
    }

    const res = await fetch(ep, {
      method: 'POST',
      headers,
      body: form.toString(),
      signal: AbortSignal.timeout(stepTimeoutMs(timeoutMs, deadline)),
      redirect: 'error',
    });
    const body = await readCapped(res, MAX_INTROSPECTION_BYTES);
    return { body, status: res.status, retryAfter: res.headers.get('Retry-After') };
  };

  /**
   * Performs one upstream introspection and, on an active 200, caches it. It is
   * the singleflight unit (the promise stored in `inflight`), so a 429 is retried
   * HERE, exactly once: a throttled cohort of identical tokens waits on one retry
   * instead of each caller retrying. The leader budget is
   * `clientAssertionTimeoutMs`, measured from the start of this call. When the
   * `Retry-After` wait cannot fit in what is left, the retry is skipped and the
   * 429 stands: running out of the leader's own budget while backing off is not
   * an endpoint failure. The retry re-resolves the client assertion, is bounded
   * by the budget left, and its own outcome (200, 429, 5xx, transport error) is
   * returned unchanged; a second 429 is never retried.
   */
  const introspectOnce = async (ep: string, token: string, key: string): Promise<UpstreamResult> => {
    const startedAt = Date.now();
    let status: number;
    let raw: string;
    try {
      let res = await introspectToken(ep, token);
      if (res.status === 429) {
        const deadline = startedAt + assertionTimeoutMs;
        const now = Date.now();
        const delayMs = introspectRetryDelayMs(res.retryAfter, now);
        if (now + delayMs >= deadline) {
          return { active: false, status: res.status };
        }
        if (delayMs > 0) {
          await new Promise<void>((resolve) => setTimeout(resolve, delayMs));
        }
        res = await introspectToken(ep, token, deadline);
      }
      status = res.status;
      raw = res.body;
    } catch (error) {
      return { active: false, status: 0, error };
    }
    if (status !== 200) {
      return { active: false, status };
    }
    const { payload, active } = parseIntrospection(raw);
    if (active && payload && cache) {
      const ttl = introspectCacheTtlMs(payload, cacheTtlMs);
      if (ttl > 0) {
        cache.set(key, payload, ttl);
      }
    }
    return { payload, active, status };
  };

  return async (ctx): Promise<Principal | undefined> => {
    const log = useLogger('security');
    const token = extractBearerToken(ctx);
    if (!token) {
      return undefined;
    }

    const ep = await resolveEndpoint();
    if (!ep) {
      log.error('token introspection unavailable: endpoint not resolved');
      return undefined;
    }

    const key = introspectCacheKey(
      ep,
      { issuer: options.issuer, clientId: options.clientId, clientSecret: options.clientSecret },
      token,
    );

    // Cache hit: re-apply the audience policy (deliberately NOT in the cache key,
    // so a cache shared across audiences enforces each resolver's on read) and
    // return a freshly-mapped, independently-owned principal.
    if (cache) {
      const cached = cache.get(key);
      if (cached && cached['active'] === true) {
        if (options.audience && !claimMatchesAudience(cached as Claims, options.audience)) {
          return undefined;
        }
        return principalFromPayload(cached);
      }
    }

    // Circuit breaker: after a run of upstream failures, fail an uncached token
    // closed immediately instead of stalling against a dead endpoint. Runs AFTER
    // the cache lookup, so tokens introspected before the outage keep resolving.
    let trial = false;
    if (breaker) {
      const decision = breaker.allow();
      if (!decision.allowed) {
        log.warn('token introspection short-circuited: circuit breaker open');
        return undefined;
      }
      trial = decision.trial;
    }

    // Collapse concurrent introspections of the same token into one upstream call.
    let promise = inflight.get(key);
    if (!promise) {
      promise = introspectOnce(ep, token, key).finally(() => inflight.delete(key));
      inflight.set(key, promise);
    }
    const result = await promise;

    // Fold the upstream outcome into the breaker (a 200 — active or inactive —
    // resets it; any error or other non-200 trips it). Recorded per admitted
    // request so a collapsed waiter still releases nothing but the leader/trial
    // frees its slot. A 429 that outlived the leader's single retry is neither:
    // the endpoint is alive and shedding load, so release() only frees a
    // half-open probe slot and leaves the failure run untouched — backpressure
    // never opens the breaker.
    if (breaker) {
      if (result.error === undefined && result.status === 429) {
        breaker.release(trial);
      } else {
        breaker.record(result.error === undefined && result.status === 200, trial);
      }
    }

    if (result.error !== undefined) {
      log.warn('token introspection request failed', {
        error: result.error instanceof Error ? result.error.message : String(result.error),
      });
      return undefined;
    }
    if (result.status === 429) {
      // The endpoint still throttled after the single bounded retry (or the
      // leader budget could not cover the Retry-After wait). Fail THIS request
      // closed — a 429 never authenticates — under a distinct log line, so shed
      // load is not mistaken for an outage.
      log.warn('token introspection throttled', { status: result.status });
      return undefined;
    }
    if (result.status === 401 || result.status === 403) {
      // The endpoint rejected THIS server's client credentials — a deployment
      // misconfiguration. The status is logged; the secret is never logged.
      log.error('token introspection rejected the resource server client credentials (check clientId/clientSecret)', {
        status: result.status,
      });
      return undefined;
    }
    if (result.status !== 200) {
      log.warn('token introspection endpoint returned an unexpected status', { status: result.status });
      return undefined;
    }
    if (!result.active || !result.payload) {
      return undefined; // inactive: never cached (introspectOnce skips it)
    }
    if (options.audience && !claimMatchesAudience(result.payload as Claims, options.audience)) {
      return undefined;
    }
    return principalFromPayload(result.payload);
  };
};
