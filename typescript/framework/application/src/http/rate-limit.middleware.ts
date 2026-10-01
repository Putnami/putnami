import type { Server } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';
import { buildTrustedProxyMatcher, type TrustedProxyMatcher } from './ip-prefix';

export interface RateLimitOptions {
  /** Time window in milliseconds. Default: `60_000` (1 minute) */
  windowMs?: number;
  /** Maximum number of requests per window. Default: `100` */
  max?: number;
  /** Function to derive the rate-limit key (e.g. IP, user id). Default: client IP from headers or socket */
  keyGenerator?: (ctx: { req: Request; headers: Headers; server?: Server }) => string;
  /** Custom response body when rate limit is exceeded */
  message?: string | object;
  /** Include standard rate-limit headers (`RateLimit-*`). Default: `true` */
  headers?: boolean;
  /**
   * Trusted reverse-proxy IPs / CIDR ranges. Each entry is an exact IP
   * (`10.0.0.1`, `::1`) or a CIDR range (`10.0.0.0/8`, `2001:db8::/32`).
   *
   * `X-Forwarded-For` is honored **only** when the direct socket peer matches a
   * trusted entry; then the rate-limit key is the first (leftmost) hop of the XFF
   * chain. An untrusted peer is always keyed by its socket address and can never
   * influence its key — trust is decided by the non-spoofable socket peer, never
   * by a header.
   *
   * Caveat: once the peer IS trusted, the leftmost hop is only as trustworthy as
   * that proxy's configuration. Configure the trusted proxy to **overwrite**
   * `X-Forwarded-For` with the real client address, not append to a
   * client-supplied value: an appending proxy (e.g. nginx
   * `$proxy_add_x_forwarded_for`, HAProxy `option forwardfor`) lets a client
   * prepend a forged hop and rotate its rate-limit key.
   *
   * Empty or unset ⇒ the socket peer is always used and `X-Forwarded-For` is
   * never trusted (the safe default). Malformed entries are skipped
   * individually.
   */
  trustedProxies?: string[];
  /** Storage strategy for rate-limit entries. Default: `'memory'` */
  storage?: RateLimitStorage;
  /** Custom store (Redis/DB). Overrides `storage`. */
  store?: RateLimitStore;
}

export type RateLimitStorage = 'memory';

export interface RateLimitStoreEntry {
  tokens: number;
  resetAt: number;
}

/** Outcome of atomically consuming one token for a rate-limit key. */
export interface RateLimitConsumeResult {
  /** Whether the request is allowed (a token was available). */
  allowed: boolean;
  /** Tokens remaining in the current window. Never negative. */
  remaining: number;
  /** Epoch milliseconds at which the current window resets. */
  resetAt: number;
}

export interface RateLimitStore {
  /**
   * Atomically consume one token for `key` and return the outcome.
   *
   * This is the operation the middleware uses, and the only one that is safe for
   * async stores. Implementations MUST apply the token accounting atomically per key: two
   * concurrent calls for the same key must never both observe the same token count. For async
   * stores (Redis/DB) that means a single atomic server-side operation — e.g. Redis `INCR` +
   * `PEXPIRE ... NX` or a Lua script, or a database upsert with `RETURNING` — never a client-side
   * get → mutate → set sequence.
   */
  consume(key: string, limit: number, windowMs: number): RateLimitConsumeResult | Promise<RateLimitConsumeResult>;
}

class MemoryRateLimitStore implements RateLimitStore {
  private readonly store = new Map<string, RateLimitStoreEntry>();
  private lastCleanup = Date.now();
  private readonly cleanupIntervalMs: number;

  constructor(readonly windowMs: number) {
    this.cleanupIntervalMs = windowMs * 2;
  }

  /**
   * Atomic by construction: a synchronous critical section on the live map — no await between
   * the read and the write, so concurrent requests can never interleave inside the accounting.
   */
  consume(key: string, limit: number, windowMs: number): RateLimitConsumeResult {
    this.cleanup();
    const now = Date.now();
    let entry = this.store.get(key);
    if (!entry || now >= entry.resetAt) {
      entry = { tokens: limit, resetAt: now + windowMs };
      this.store.set(key, entry);
    }
    entry.tokens -= 1;
    return { allowed: entry.tokens >= 0, remaining: Math.max(0, entry.tokens), resetAt: entry.resetAt };
  }

  private cleanup() {
    const now = Date.now();
    if (now - this.lastCleanup < this.cleanupIntervalMs) return;
    this.lastCleanup = now;
    for (const [k, entry] of this.store) {
      if (now >= entry.resetAt) this.store.delete(k);
    }
  }
}

type KeyGeneratorContext = { req: Request; headers: Headers; server?: Server };

function socketKeyGenerator(ctx: KeyGeneratorContext): string {
  return connectionIP(ctx.server, ctx.req) || 'unknown';
}

/**
 * Key generator that ports Go's `buildRateLimitKeyFunc` trusted-proxy semantics:
 * `X-Forwarded-For` is honored only when the direct socket peer matches a
 * trusted exact IP or CIDR, in which case the key is the first XFF hop
 * (comma-split, trimmed). Otherwise — including an untrusted peer, an empty
 * matcher, or an unparseable peer — the key is the socket peer, so XFF can never
 * be spoofed to forge the key.
 */
export function trustedProxyKeyGenerator(matcher: TrustedProxyMatcher): (ctx: KeyGeneratorContext) => string {
  return (ctx: KeyGeneratorContext): string => {
    const peer = connectionIP(ctx.server, ctx.req);
    if (matcher.matches(peer)) {
      const forwarded = ctx.headers.get('X-Forwarded-For');
      // Mirror Go: only a non-empty header is consulted. Take the first
      // comma-separated hop and trim it — which may itself be empty for a
      // whitespace-only header — without falling back to the peer once trusted.
      if (forwarded) {
        return firstForwardedHop(forwarded);
      }
    }
    return peer || 'unknown';
  };
}

function firstForwardedHop(forwarded: string): string {
  const comma = forwarded.indexOf(',');
  const first = comma === -1 ? forwarded : forwarded.slice(0, comma);
  return first.trim();
}

/** Resolve the effective spoof-resistant key generator. */
function selectKeyGenerator(options: RateLimitOptions): (ctx: KeyGeneratorContext) => string {
  if (options.keyGenerator) {
    return options.keyGenerator;
  }
  if (options.trustedProxies !== undefined) {
    return trustedProxyKeyGenerator(buildTrustedProxyMatcher(options.trustedProxies));
  }
  return socketKeyGenerator;
}

function connectionIP(server: Server | undefined, req: Request): string | undefined {
  const ip = server?.requestIP?.(req);
  return ip?.address;
}

export const RateLimitMiddleware = (options: RateLimitOptions = {}): HttpMiddleware => {
  const windowMs = options.windowMs ?? 60_000;
  const max = options.max ?? 100;
  const keyGenerator = selectKeyGenerator(options);
  const includeHeaders = options.headers ?? true;
  const message = options.message ?? { error: 'Too Many Requests' };
  const store =
    options.store ??
    (options.storage === 'memory' || !options.storage ? new MemoryRateLimitStore(windowMs) : undefined);
  if (!store) {
    throw new Error('RateLimitMiddleware requires a store. Provide options.store for custom storage.');
  }

  return async (ctx, next) => {
    const key = keyGenerator(ctx);
    const outcome = await store.consume(key, max, windowMs);

    if (!outcome.allowed) {
      const retryAfter = Math.max(1, Math.ceil((outcome.resetAt - Date.now()) / 1000));
      let res = HttpResponse.json(typeof message === 'string' ? { error: message } : message, { status: 429 });
      res = res.setHeader('Retry-After', String(retryAfter));
      if (includeHeaders) {
        res = res.setHeader('RateLimit-Limit', String(max));
        res = res.setHeader('RateLimit-Remaining', '0');
        res = res.setHeader('RateLimit-Reset', String(Math.ceil(outcome.resetAt / 1000)));
      }
      return res;
    }

    let res = await next();
    if (!res) return res;

    if (includeHeaders) {
      res = res.setHeader('RateLimit-Limit', String(max));
      res = res.setHeader('RateLimit-Remaining', String(outcome.remaining));
      res = res.setHeader('RateLimit-Reset', String(Math.ceil(outcome.resetAt / 1000)));
    }
    return res;
  };
};
