import { createHmac, timingSafeEqual } from 'node:crypto';
import { DEFAULT_MAX_BODY_SIZE_BYTES, ensureBodyWithinLimit } from './http-context.builder';
import type { HttpRequestContextInternal } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';

export interface CsrfOptions {
  /** Name of the cookie that stores the CSRF token. Default: `'_csrf'` */
  cookieName?: string;
  /** Name of the header the client must send. Default: `'X-CSRF-Token'` */
  headerName?: string;
  /** Name of the form body field that can carry the CSRF token. Default: `'_csrf'` */
  fieldName?: string;
  /** HTTP methods that are exempt from CSRF checks (safe methods). Default: `['GET','HEAD','OPTIONS']` */
  ignoreMethods?: string[];
  /** SameSite attribute for the CSRF cookie. Default: `'Strict'` */
  sameSite?: 'Strict' | 'Lax' | 'None';
  /** Restrict cookie to HTTPS. Default: `true` */
  secure?: boolean;
  /** CSRF storage strategy. Default: `'cookie'` */
  storage?: CsrfStorage;
  /** Custom CSRF store (for Redis/DB). Overrides `storage`. */
  store?: CsrfStore;
  /** Optional signing secret for cookie values. */
  secret?: string;
  /** Optional TTL for CSRF tokens (used by memory/custom stores). */
  ttlMs?: number;
  /** Optional Max-Age for the CSRF cookie (seconds). */
  cookieMaxAge?: number;
  /**
   * Maximum request body size (bytes) when extracting the CSRF token from a
   * form body — the same guard `ctx.body()` applies before buffering. The HTTP
   * plugin threads its resolved `maxBodySizeBytes` in automatically; an explicit
   * value here overrides it. `0` disables the limit (not recommended).
   * Default: `1_048_576` (1 MiB).
   */
  maxBodySizeBytes?: number;
}

export type CsrfStorage = 'cookie' | 'memory';

/**
 * Request-context slot where the middleware publishes the effective CSRF token
 * for the current request (the validated cookie token, or the freshly generated
 * one the response's `Set-Cookie` will carry). SSR renderers read it so the
 * token can be embedded (e.g. as a hidden `_csrf` form field) on the very
 * first response — before the client has ever received the cookie — which
 * completes the no-JavaScript form round-trip.
 */
export const CSRF_TOKEN_CONTEXT_KEY = 'csrfToken';

const CSRF_MIDDLEWARE_MARKER = 'putnami:csrf-middleware' as const;

type MarkedCsrfMiddleware = HttpMiddleware & {
  readonly __csrfMiddleware: typeof CSRF_MIDDLEWARE_MARKER;
};

/**
 * Identifies middleware created by {@link CsrfMiddleware}.
 *
 * The string marker deliberately survives duplicate package instances, which
 * can occur when generated React routes and the root HTTP plugin resolve the
 * framework from different locations.
 */
export function isCsrfMiddleware(value: unknown): value is HttpMiddleware {
  return (
    typeof value === 'function' &&
    (value as unknown as Partial<MarkedCsrfMiddleware>).__csrfMiddleware === CSRF_MIDDLEWARE_MARKER
  );
}

export interface CsrfStoreEntry {
  expiresAt?: number;
}

export interface CsrfStore {
  get(token: string): CsrfStoreEntry | undefined | Promise<CsrfStoreEntry | undefined>;
  set(token: string, entry: CsrfStoreEntry): void | Promise<void>;
  delete?(token: string): void | Promise<void>;
}

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);

const DEFAULT_TTL_MS = 60 * 60 * 1000;

function generateToken(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return Buffer.from(bytes).toString('hex');
}

class MemoryCsrfStore implements CsrfStore {
  private readonly store = new Map<string, number>();
  private lastCleanup = Date.now();
  private readonly ttlMs: number;
  private readonly cleanupIntervalMs: number;

  constructor(ttlMs?: number) {
    this.ttlMs = ttlMs ?? DEFAULT_TTL_MS;
    this.cleanupIntervalMs = this.ttlMs * 2;
  }

  get(token: string): CsrfStoreEntry | undefined {
    this.cleanup();
    const expiresAt = this.store.get(token);
    if (!expiresAt) return undefined;
    if (expiresAt <= Date.now()) {
      this.store.delete(token);
      return undefined;
    }
    return { expiresAt };
  }

  set(token: string, entry: CsrfStoreEntry): void {
    this.cleanup();
    const expiresAt = entry.expiresAt ?? Date.now() + this.ttlMs;
    this.store.set(token, expiresAt);
  }

  delete(token: string): void {
    this.store.delete(token);
  }

  private cleanup() {
    const now = Date.now();
    if (now - this.lastCleanup < this.cleanupIntervalMs) return;
    this.lastCleanup = now;
    for (const [key, expiresAt] of this.store) {
      if (expiresAt <= now) {
        this.store.delete(key);
      }
    }
  }
}

const BASE64URL_RE = /[+/=]/g;
const BASE64URL_MAP: Record<string, string> = { '+': '-', '/': '_', '=': '' };

function base64ToBase64url(s: string): string {
  return s.replace(BASE64URL_RE, (c) => BASE64URL_MAP[c]);
}

/**
 * Constant-time string comparison. Returns false for length mismatches without
 * the early throw that `timingSafeEqual` raises on unequal-length buffers.
 */
function constantTimeEqual(a: string, b: string): boolean {
  const bufA = Buffer.from(a);
  const bufB = Buffer.from(b);
  if (bufA.length !== bufB.length) {
    // Still run a comparison against a same-length buffer to avoid an obvious
    // fast-path on length; the result is discarded.
    timingSafeEqual(bufA, bufA);
    return false;
  }
  return timingSafeEqual(bufA, bufB);
}

function signToken(token: string, secret: string): string {
  const signature = createHmac('sha256', secret).update(token).digest('base64');
  return `${token}.${base64ToBase64url(signature)}`;
}

function verifySignedToken(value: string, secret: string): string | undefined {
  const idx = value.lastIndexOf('.');
  if (idx <= 0) return undefined;
  const token = value.slice(0, idx);
  const signature = value.slice(idx + 1);
  const expected = base64ToBase64url(createHmac('sha256', secret).update(token).digest('base64'));
  if (signature.length !== expected.length) return undefined;
  if (!timingSafeEqual(Buffer.from(signature), Buffer.from(expected))) {
    return undefined;
  }
  return token;
}

function parseCookieToken(value: string | undefined, secret?: string): string | undefined {
  if (!value) return undefined;
  if (!secret) return value;
  return verifySignedToken(value, secret);
}

function normalizeHeaderToken(
  headerToken: string | null,
  cookieValue: string | undefined,
  secret?: string,
): string | undefined {
  if (!headerToken) return undefined;
  if (!secret) return headerToken;
  if (cookieValue && headerToken === cookieValue) {
    return verifySignedToken(headerToken, secret);
  }
  return headerToken;
}

const FORM_CONTENT_TYPES = new Set(['application/x-www-form-urlencoded', 'multipart/form-data']);

async function extractBodyToken(
  req: Request,
  fieldName: string,
  maxBodySizeBytes: number,
): Promise<string | undefined> {
  const contentTypeHeader = req.headers.get('Content-Type');
  if (!contentTypeHeader) return undefined;
  const contentType = contentTypeHeader.split(';')[0]?.trim().toLowerCase() ?? '';
  if (!FORM_CONTENT_TYPES.has(contentType)) return undefined;
  const cloned = req.clone();
  // Enforce the same body-size limit as ctx.body()/parseBody BEFORE buffering
  // the form. Deliberately outside the try/catch below: an oversized body must
  // surface as the same 413 HttpAbortException parseBody raises — never buffer
  // an unbounded attacker-controlled body, and never degrade the rejection into
  // a 403 CSRF mismatch.
  await ensureBodyWithinLimit(cloned, maxBodySizeBytes);
  try {
    const form = await cloned.formData();
    const value = form.get(fieldName);
    return typeof value === 'string' ? value : undefined;
  } catch {
    return undefined;
  }
}

export const CsrfMiddleware = (options: CsrfOptions = {}): HttpMiddleware => {
  const cookieName = options.cookieName || '_csrf';
  const headerName = options.headerName || 'X-CSRF-Token';
  const fieldName = options.fieldName || '_csrf';
  const ignoreMethods = new Set(options.ignoreMethods || SAFE_METHODS);
  const sameSite = options.sameSite || 'Strict';
  const secure = options.secure ?? true;
  const secret = options.secret;
  const ttlMs = options.ttlMs ?? DEFAULT_TTL_MS;
  const store = options.store ?? (options.storage === 'memory' ? new MemoryCsrfStore(ttlMs) : undefined);
  const cookieMaxAge = options.cookieMaxAge;
  const maxBodySizeBytes = options.maxBodySizeBytes ?? DEFAULT_MAX_BODY_SIZE_BYTES;

  const middleware: HttpMiddleware = async (ctx, next) => {
    const matchedHandlers = (ctx as HttpRequestContextInternal).__matchedHandlers;
    // Route matching normally runs before middleware. An explicitly empty match
    // belongs to the router's 404/405 handling, while missing match metadata
    // still fails closed for standalone middleware invocation.
    if (matchedHandlers?.length === 0 || matchedHandlers?.[0]?.csrfExempt) {
      return next();
    }

    const cookieHeader = ctx.req.headers.get('Cookie');
    // Native parsing via Bun.CookieMap; URI-decoding is a no-op for our token
    // payloads (hex token + base64url signature contain no percent-encoded chars).
    const cookieValue = cookieHeader ? (new Bun.CookieMap(cookieHeader).get(cookieName) ?? undefined) : undefined;
    const methodIsSafe = ignoreMethods.has(ctx.method);

    let csrfToken = parseCookieToken(cookieValue, secret);
    let stored = csrfToken && store ? await store.get(csrfToken) : undefined;
    const now = Date.now();

    if (stored?.expiresAt && stored.expiresAt <= now) {
      if (store?.delete && csrfToken) {
        await store.delete(csrfToken);
      }
      stored = undefined;
    }

    const hasValidToken = Boolean(csrfToken) && (!store || stored);

    if (!hasValidToken) {
      if (!methodIsSafe) {
        return HttpResponse.json({ error: 'CSRF token mismatch' }, { status: 403 });
      }
      csrfToken = generateToken();
      if (store) {
        await store.set(csrfToken, { expiresAt: now + ttlMs });
      }
    }

    // Validate on unsafe methods
    if (!methodIsSafe) {
      let submittedToken = normalizeHeaderToken(ctx.req.headers.get(headerName), cookieValue, secret);
      // Fall back to form body field when no header is present (plain HTML form submissions)
      if (!submittedToken) {
        const bodyToken = await extractBodyToken(ctx.req, fieldName, maxBodySizeBytes);
        submittedToken = normalizeHeaderToken(bodyToken ?? null, cookieValue, secret);
      }
      if (!submittedToken || !csrfToken || !constantTimeEqual(submittedToken, csrfToken)) {
        return HttpResponse.json({ error: 'CSRF token mismatch' }, { status: 403 });
      }
      if (store && !stored) {
        return HttpResponse.json({ error: 'CSRF token mismatch' }, { status: 403 });
      }
    }

    // Publish the effective token for SSR renderers (see CSRF_TOKEN_CONTEXT_KEY)
    // before the handler runs, so the first-ever response can render the token
    // its own Set-Cookie is about to establish.
    if (csrfToken) {
      (ctx as unknown as Record<string, unknown>)[CSRF_TOKEN_CONTEXT_KEY] = csrfToken;
    }

    let res = await next();
    if (!res) return res;

    if (!csrfToken) return res;

    // Keep an established client token stable. Re-emitting an unchanged cookie
    // is redundant and lets concurrent responses unnecessarily compete to update
    // the browser's cookie jar.
    const cookiePayload = secret ? signToken(csrfToken, secret) : csrfToken;
    if (cookiePayload === cookieValue) return res;

    const cookieParts = [`${cookieName}=${cookiePayload}`, 'Path=/', `SameSite=${sameSite}`];
    if (cookieMaxAge !== undefined) {
      cookieParts.push(`Max-Age=${cookieMaxAge}`);
    }
    if (secure) cookieParts.push('Secure');
    // Not HttpOnly — client JS must read the token to send it back
    res = res.pushHeader('Set-Cookie', cookieParts.join('; '));
    return res;
  };

  return Object.assign(middleware, { __csrfMiddleware: CSRF_MIDDLEWARE_MARKER });
};
