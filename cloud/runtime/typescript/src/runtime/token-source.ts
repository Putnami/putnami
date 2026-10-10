import { useLogger } from '@putnami/runtime';
import { syncFetch } from './sync-fetch';

/**
 * Resolves the bearer token sent on each request to the config / secrets
 * resolve endpoints. Implementations may cache, refresh, or read env on
 * every call — callers must not assume a stable string.
 *
 * `token()` is synchronous to match `ConfigSource.load()`.
 */
export interface TokenSource {
  token(): string | undefined;
}

/**
 * Reads a bearer token from the named environment variable on every
 * call. Preserves the `CONFIG_SERVER_TOKEN` behavior for non-cloud
 * runtimes.
 */
export class EnvVarTokenSource implements TokenSource {
  constructor(private readonly name: string) {}

  token(): string | undefined {
    const v = process.env[this.name]?.trim();
    return v || undefined;
  }
}

/**
 * Fetches a GCP ID token from the metadata server, bound to the
 * configured audience. The first `token()` call performs an in-process
 * synchronous fetch; subsequent calls within the cached TTL return the
 * in-memory copy.
 *
 * Refresh: tokens valid until `exp - 5min` are served from cache. The
 * fallback TTL when `exp` cannot be parsed is 50min, matching the Go
 * implementation (long enough to amortize the round trip, short enough
 * to avoid serving a stale token).
 */
export class GcpMetadataTokenSource implements TokenSource {
  private cached: string | undefined;
  private expiry: number = 0; // epoch seconds

  constructor(
    private readonly audience: string,
    private readonly timeoutMs: number = 2000,
  ) {}

  token(): string | undefined {
    const nowSec = Math.floor(Date.now() / 1000);
    if (this.cached && this.expiry - nowSec > 300) {
      return this.cached;
    }
    const tok = this.fetch();
    if (!tok) {
      return undefined;
    }
    const exp = parseJWTExpiry(tok);
    this.cached = tok;
    this.expiry = exp || nowSec + 50 * 60;
    return tok;
  }

  private fetch(): string | undefined {
    try {
      const url = new URL(
        'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity',
      );
      url.searchParams.set('audience', this.audience);
      url.searchParams.set('format', 'full');
      const response = syncFetch({
        url: url.toString(),
        method: 'GET',
        headers: { 'Metadata-Flavor': 'Google' },
        timeoutMs: this.timeoutMs,
      });
      const token = response.body.trim();
      return token || undefined;
    } catch {
      useLogger('config-server').warn('metadata token fetch failed', { audience: this.audience });
      return undefined;
    }
  }
}

/**
 * Parses the `exp` claim from an unverified JWT payload. Verification is
 * the resource server's job; we only need the expiry to drive cache
 * busting. Returns 0 on any failure — callers fall back to a
 * conservative default.
 */
export function parseJWTExpiry(token: string): number {
  const parts = token.split('.');
  if (parts.length < 2) return 0;
  const payload = parts[1];
  if (!payload) return 0;
  try {
    const decoded = base64UrlDecode(payload);
    const claims = JSON.parse(decoded) as { exp?: number };
    return typeof claims.exp === 'number' ? claims.exp : 0;
  } catch {
    return 0;
  }
}

function base64UrlDecode(s: string): string {
  const padded = s.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((s.length + 3) % 4);
  return Buffer.from(padded, 'base64').toString('utf-8');
}

/**
 * Env vars discovery checks for an operator-provided bearer, in
 * precedence order (most-specific first). The Go runtime uses the same
 * list, so a workspace's env-var setup behaves identically across
 * runtimes.
 */
export const CONFIG_SERVER_TOKEN_ENV_NAMES = [
  'PUTNAMI_CLOUD_TOKEN', // explicit: cloud control-plane bearer
  'CONFIG_SERVER_TOKEN', // legacy: original framework name
  'PUTNAMI_TOKEN', // generic Putnami token, broad fallback
] as const;

const gcpTokenSourceCache = new Map<string, GcpMetadataTokenSource>();

/**
 * Picks the bearer-token resolver for both config-server and
 * secrets-server discovery.
 *
 * Precedence is fixed: operator override (env var) > GCP workload identity >
 * unauthenticated. Detection is env-var-based (`K_SERVICE`,
 * `GOOGLE_CLOUD_PROJECT`) per the protocol contract. GCP metadata ID tokens
 * use `CONFIG_SERVER_AUDIENCE` when set, otherwise `CONFIG_SERVER_URL`.
 */
export function discoverTokenSource(serverUrl: string): TokenSource {
  for (const name of CONFIG_SERVER_TOKEN_ENV_NAMES) {
    if (process.env[name]?.trim()) {
      return new EnvVarTokenSource(name);
    }
  }
  if (process.env['K_SERVICE'] || process.env['GOOGLE_CLOUD_PROJECT']) {
    const audience = configServerAudience(serverUrl);
    let source = gcpTokenSourceCache.get(audience);
    if (!source) {
      source = new GcpMetadataTokenSource(audience);
      gcpTokenSourceCache.set(audience, source);
    }
    return source;
  }
  useLogger('config-server').warn(
    'CONFIG_SERVER_URL is set but no credentials are configured (PUTNAMI_CLOUD_TOKEN / CONFIG_SERVER_TOKEN / PUTNAMI_TOKEN unset and no GCP workload identity detected); resolve calls will be unauthenticated and may be rejected',
  );
  return new EnvVarTokenSource('CONFIG_SERVER_TOKEN');
}

export function resetTokenSourceCacheForTest(): void {
  gcpTokenSourceCache.clear();
}

function configServerAudience(serverUrl: string): string {
  return process.env['CONFIG_SERVER_AUDIENCE']?.trim() || serverUrl;
}
