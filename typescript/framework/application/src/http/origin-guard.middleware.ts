import { useLogger } from '@putnami/runtime';
import type { HttpRequestContext } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';
import { isMinimalRoute } from './minimal.util';

export interface OriginGuardOptions {
  /** Origins to trust for cross-origin requests (e.g. `'https://admin.example.com'`). */
  trustedOrigins?: string[];
  /** Allow same-site requests (subdomains of the same registrable domain). Default: `true`. */
  allowSameSite?: boolean;
  /** `'enforce'` blocks violations (default). `'report'` logs a warning but allows the request through. */
  mode?: 'enforce' | 'report';
}

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);

/** Creates a middleware that blocks cross-origin state-changing requests unless the origin is trusted. */
export function OriginGuardMiddleware(options: OriginGuardOptions = {}): HttpMiddleware {
  const trustedHosts = new Set<string>();
  for (const origin of options.trustedOrigins ?? []) {
    const host = extractHostFromOrigin(origin);
    if (host) trustedHosts.add(host);
  }
  const allowSameSite = options.allowSameSite ?? true;
  const enforce = (options.mode ?? 'enforce') === 'enforce';

  return async (ctx, next) => {
    // Per-route opt-out: skip only when every matched candidate is minimal.
    if (isMinimalRoute(ctx)) return next();

    if (SAFE_METHODS.has(ctx.method)) return next();

    if (isOriginAllowed(ctx, trustedHosts, allowSameSite)) return next();

    if (!enforce) {
      useLogger('origin-guard').warn('Cross-origin request would be blocked', {
        method: ctx.method,
        path: ctx.path(),
        origin: ctx.headers.get('Origin'),
        secFetchSite: ctx.headers.get('Sec-Fetch-Site'),
        host: ctx.host(),
      });
      return next();
    }

    return HttpResponse.json({ error: 'Cross-origin request blocked' }, { status: 403 });
  };
}

function isOriginAllowed(ctx: HttpRequestContext, trustedHosts: Set<string>, allowSameSite: boolean): boolean {
  const secFetchSite = ctx.headers.get('Sec-Fetch-Site');

  if (secFetchSite) {
    if (secFetchSite === 'same-origin' || secFetchSite === 'none') return true;
    if (secFetchSite === 'same-site' && allowSameSite) return true;
    // cross-site (or same-site when disallowed) — check trusted origins
    const origin = ctx.headers.get('Origin');
    if (origin) {
      const host = extractHostFromOrigin(origin);
      if (host && trustedHosts.has(host)) return true;
    }
    return false;
  }

  // Fallback: Origin vs Host
  const origin = ctx.headers.get('Origin');
  if (!origin) return true; // non-browser client
  const originHost = extractHostFromOrigin(origin);
  if (originHost === ctx.host()) return true;
  if (originHost && trustedHosts.has(originHost)) return true;
  return false;
}

function extractHostFromOrigin(origin: string): string | undefined {
  const doubleSlash = origin.indexOf('//');
  if (doubleSlash === -1) return undefined;
  const afterScheme = origin.slice(doubleSlash + 2);
  const slashIdx = afterScheme.indexOf('/');
  return slashIdx === -1 ? afterScheme : afterScheme.slice(0, slashIdx);
}
