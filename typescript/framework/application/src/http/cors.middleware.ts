import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';

export interface CorsOptions {
  /** Allowed origins. Use `'*'` to allow all, or provide specific origins. Default: `'*'` */
  origin?: string | string[] | ((origin: string) => boolean);
  /** Allowed HTTP methods. Default: `['GET','HEAD','PUT','PATCH','POST','DELETE']` */
  methods?: string[];
  /** Headers the client is allowed to send. Default: reflects the request's `Access-Control-Request-Headers` */
  allowedHeaders?: string[];
  /** Headers exposed to the client. Default: none */
  exposedHeaders?: string[];
  /** Allow credentials (cookies, authorization headers). Default: `false` */
  credentials?: boolean;
  /** `Access-Control-Max-Age` in seconds for preflight cache. Default: none */
  maxAge?: number;
}

const DEFAULT_METHODS = ['GET', 'HEAD', 'PUT', 'PATCH', 'POST', 'DELETE'];

function resolveOrigin(option: CorsOptions['origin'], requestOrigin: string | null): string | undefined {
  if (!requestOrigin) return undefined;
  if (option === '*' || option === undefined) return '*';
  if (typeof option === 'string') {
    return option === requestOrigin ? option : undefined;
  }
  if (typeof option === 'function') {
    return option(requestOrigin) ? requestOrigin : undefined;
  }
  if (Array.isArray(option)) {
    return option.includes(requestOrigin) ? requestOrigin : undefined;
  }
  return undefined;
}

export const CorsMiddleware = (options: CorsOptions = {}): HttpMiddleware => {
  if ((options.origin === '*' || options.origin === undefined) && options.credentials) {
    throw new Error(
      'CORS: origin "*" cannot be used with credentials:true. Provide explicit origins or an origin function.',
    );
  }

  const methods = (options.methods || DEFAULT_METHODS).join(', ');

  return async (ctx, next) => {
    const requestOrigin = ctx.req.headers.get('Origin');
    const allowedOrigin = resolveOrigin(options.origin, requestOrigin);

    // Preflight request
    if (ctx.req.method === 'OPTIONS') {
      let res = new HttpResponse(undefined, { status: 204 });
      if (allowedOrigin) {
        res = res.setHeader('Access-Control-Allow-Origin', allowedOrigin);
      }
      res = res.setHeader('Access-Control-Allow-Methods', methods);

      const allowedHeaders =
        options.allowedHeaders?.join(', ') || ctx.req.headers.get('Access-Control-Request-Headers') || '';
      if (allowedHeaders) {
        res = res.setHeader('Access-Control-Allow-Headers', allowedHeaders);
      }
      if (options.credentials) {
        res = res.setHeader('Access-Control-Allow-Credentials', 'true');
      }
      if (options.maxAge !== undefined) {
        res = res.setHeader('Access-Control-Max-Age', String(options.maxAge));
      }
      return res;
    }

    // Actual request
    let res = await next();
    if (!res) return res;

    if (allowedOrigin) {
      res = res.setHeader('Access-Control-Allow-Origin', allowedOrigin);
    }
    if (options.credentials) {
      res = res.setHeader('Access-Control-Allow-Credentials', 'true');
    }
    if (options.exposedHeaders?.length) {
      res = res.setHeader('Access-Control-Expose-Headers', options.exposedHeaders.join(', '));
    }
    if (allowedOrigin && allowedOrigin !== '*') {
      res = res.setHeader('Vary', 'Origin');
    }
    return res;
  };
};
