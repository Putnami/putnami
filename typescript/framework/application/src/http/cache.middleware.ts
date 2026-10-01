import { createHash } from 'node:crypto';
import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';

export interface HttpCacheOptions {
  /** Raw Cache-Control directive string. Overrides all shorthands when provided. */
  cacheControl?: string;
  /** Sets `public, max-age=<value>` */
  maxAge?: number;
  /** Sets `private, max-age=<value>` */
  privateMaxAge?: number;
  /** Appends `s-maxage=<value>` to the Cache-Control header */
  sMaxAge?: number;
  /** Appends `stale-while-revalidate=<value>` to the Cache-Control header */
  staleWhileRevalidate?: number;
  /** Enable ETag generation. `true` uses MD5 hash; a function receives the serialized body and returns a custom ETag string. */
  etag?: boolean | ((body: string) => string);
}

export function buildCacheControl(options: HttpCacheOptions): string {
  if (options.cacheControl) return options.cacheControl;

  const directives: string[] = [];
  if (options.privateMaxAge !== undefined) {
    directives.push('private', `max-age=${options.privateMaxAge}`);
  } else if (options.maxAge !== undefined) {
    directives.push('public', `max-age=${options.maxAge}`);
  }
  if (options.sMaxAge !== undefined) {
    directives.push(`s-maxage=${options.sMaxAge}`);
  }
  if (options.staleWhileRevalidate !== undefined) {
    directives.push(`stale-while-revalidate=${options.staleWhileRevalidate}`);
  }
  return directives.join(', ');
}

export function computeETag(body: string): string {
  const hash = createHash('md5').update(body).digest('hex').substring(0, 16);
  return `"${hash}"`;
}

export const CacheMiddleware =
  (options: HttpCacheOptions = {}): HttpMiddleware =>
  async (ctx, next) => {
    let res = await next();
    if (!res) return res;

    const cacheControl = buildCacheControl(options);
    if (cacheControl) {
      res = res.setHeader('Cache-Control', cacheControl);
    }

    if (options.etag) {
      let etag = res.getHeader('ETag');

      // Auto-generate ETag from response body when not already set
      if (!etag) {
        const body = res.getBodyInit();
        if (typeof body === 'string') {
          etag = typeof options.etag === 'function' ? options.etag(body) : computeETag(body);
          res = res.setHeader('ETag', etag);
        }
      }

      if (etag && ctx.headers.get('If-None-Match') === etag) {
        const body = res.getBodyInit();
        if (body instanceof ReadableStream) await body.cancel().catch(() => {});
        return new HttpResponse(undefined, { status: 304, headers: { ETag: etag } });
      }
    }

    return res;
  };
