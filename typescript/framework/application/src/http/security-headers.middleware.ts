import type { HttpRequestContext } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import type { HttpResponse } from './http-response';
import { skipsSecurityHeaders } from './minimal.util';
import { actsOnRoutes } from './route-scope';

export const CSP_NONCE_CONTEXT_KEY = 'cspNonce';

export interface SecurityHeadersOptions {
  /** Content-Security-Policy directive. Set to `false` to omit. */
  contentSecurityPolicy?: string | false;
  /** X-Content-Type-Options value. Default: `'nosniff'`. Set to `false` to omit. */
  contentTypeOptions?: string | false;
  /** X-Frame-Options value. Default: `'DENY'`. Set to `false` to omit. */
  frameOptions?: string | false;
  /** Referrer-Policy value. Default: `'strict-origin-when-cross-origin'`. Set to `false` to omit. */
  referrerPolicy?: string | false;
  /** Permissions-Policy value. Default: restrictive policy. Set to `false` to omit. */
  permissionsPolicy?: string | false;
  /** Strict-Transport-Security value. Default: `'max-age=31536000; includeSubDomains'`. Set to `false` to omit. */
  strictTransportSecurity?: string | false;
  /** X-XSS-Protection value. Default: `'0'` (disabled in favor of CSP). Set to `false` to omit. */
  xssProtection?: string | false;
}

const defaults: Required<SecurityHeadersOptions> = {
  contentSecurityPolicy:
    "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
  contentTypeOptions: 'nosniff',
  frameOptions: 'DENY',
  referrerPolicy: 'strict-origin-when-cross-origin',
  permissionsPolicy: 'camera=(), microphone=(), geolocation=(), payment=()',
  strictTransportSecurity: 'max-age=31536000; includeSubDomains',
  xssProtection: '0',
};

const headerMap: Record<keyof SecurityHeadersOptions, string> = {
  contentSecurityPolicy: 'Content-Security-Policy',
  contentTypeOptions: 'X-Content-Type-Options',
  frameOptions: 'X-Frame-Options',
  referrerPolicy: 'Referrer-Policy',
  permissionsPolicy: 'Permissions-Policy',
  strictTransportSecurity: 'Strict-Transport-Security',
  xssProtection: 'X-XSS-Protection',
};

function withCspNonce(value: string, nonce: unknown): string {
  if (typeof nonce !== 'string' || nonce.length === 0) return value;

  const nonceDirective = `'nonce-${nonce}'`;
  if (value.includes(nonceDirective)) return value;

  const directives = value
    .split(';')
    .map((directive) => directive.trim())
    .filter(Boolean);
  const scriptSrcIndex = directives.findIndex(
    (directive) => directive.toLowerCase().split(/\s+/, 1)[0] === 'script-src',
  );

  if (scriptSrcIndex >= 0) {
    directives[scriptSrcIndex] = `${directives[scriptSrcIndex]} ${nonceDirective}`;
  } else {
    directives.push(`script-src 'self' ${nonceDirective}`);
  }

  return directives.join('; ');
}

/** A security header resolved once at middleware construction. */
interface PrecomputedHeader {
  /** Canonical header name (e.g. `X-Frame-Options`). */
  name: string;
  /** Lowercased name used to detect headers already set by inner middleware. */
  lowerName: string;
  /** Static header value (for CSP, the policy *without* a per-request nonce). */
  value: string;
  /** When `true`, the option was set explicitly and always overrides an inner header. */
  forced: boolean;
  /** Only the CSP header weaves a per-request nonce into its value. */
  isCsp: boolean;
  /** Only emit over HTTPS (HSTS is ignored over plaintext per RFC 6797). */
  httpsOnly: boolean;
}

/**
 * The policy a JSON response carries in place of the document one: a JSON body
 * is never rendered as a document, so only framing needs forbidding.
 */
const JSON_CONTENT_SECURITY_POLICY = "frame-ancestors 'none'";

/**
 * Whether a Content-Type names JSON: `application/json` or a `+json` suffix
 * type such as `application/problem+json`, with any parameters.
 */
export function isJsonContentType(contentType: string | undefined): boolean {
  if (!contentType) return false;
  const semi = contentType.indexOf(';');
  const mediaType = (semi >= 0 ? contentType.slice(0, semi) : contentType).trim().toLowerCase();
  return mediaType === 'application/json' || mediaType.endsWith('+json');
}

/**
 * The header set of a JSON response: nosniff, CSP reduced to frame-ancestors
 * 'none', HSTS, and every header an option set explicitly. The document-only
 * defaults (X-Frame-Options, Referrer-Policy, Permissions-Policy, X-XSS-Protection)
 * add nothing to a body no browser renders.
 */
function jsonHeaderSet(precomputed: readonly PrecomputedHeader[]): PrecomputedHeader[] {
  const set: PrecomputedHeader[] = [];
  for (const h of precomputed) {
    if (h.forced || h.httpsOnly || h.lowerName === 'x-content-type-options') {
      set.push(h);
    } else if (h.isCsp) {
      set.push({ ...h, value: JSON_CONTENT_SECURITY_POLICY, isCsp: false });
    }
  }
  return set;
}

/** A prebuilt response header list for the commonest response: JSON with no other header. */
interface HeaderTemplate {
  entries: readonly [string, string][];
  headers: Headers;
}

function headerTemplate(contentType: string, set: readonly PrecomputedHeader[], secured: boolean): HeaderTemplate {
  const entries: [string, string][] = [['Content-Type', contentType]];
  for (const h of set) {
    if (h.httpsOnly && !secured) continue;
    entries.push([h.name, h.value]);
  }
  return { entries, headers: new Headers(entries) };
}

export const SecurityHeadersMiddleware = (options: SecurityHeadersOptions = {}): HttpMiddleware => {
  // Resolve the full header set once, at construction. The merged config never
  // changes between requests, so the only per-request work left is an optional
  // CSP-nonce weave plus the "don't clobber inner headers" check.
  const merged = { ...defaults, ...options };
  const precomputed: PrecomputedHeader[] = [];
  for (const key of Object.keys(headerMap) as (keyof SecurityHeadersOptions)[]) {
    const value = merged[key];
    if (value === false || value === undefined) continue;
    const name = headerMap[key];
    precomputed.push({
      name,
      lowerName: name.toLowerCase(),
      value: value as string,
      forced: Object.hasOwn(options, key),
      isCsp: key === 'contentSecurityPolicy',
      httpsOnly: key === 'strictTransportSecurity',
    });
  }

  const jsonSet = jsonHeaderSet(precomputed);
  // A forced CSP may weave a per-request nonce, so only a nonce-free set gets a template.
  const templated = !jsonSet.some((h) => h.isCsp);
  const jsonTemplates = templated
    ? {
        plain: headerTemplate('application/json', jsonSet, false),
        secured: headerTemplate('application/json', jsonSet, true),
      }
    : undefined;

  /** Applies the header set to the response the inner chain produced. */
  const secure = (ctx: HttpRequestContext, response: HttpResponse | undefined): HttpResponse | undefined => {
    if (!response) return response;
    let res = response;

    const raw = res.rawHeaderEntries();
    // The commonest response — JSON whose only header is its Content-Type —
    // takes a header list built once, at construction.
    if (jsonTemplates && raw.length === 1 && raw[0][0] === 'Content-Type' && raw[0][1] === 'application/json') {
      const template = ctx.secured() ? jsonTemplates.secured : jsonTemplates.plain;
      return res.useHeaderTemplate(template.entries, template.headers);
    }

    // Map lowercased existing header name -> original casing, built only when
    // the inner response actually carries headers (the common JSON case has
    // just Content-Type). Lets us both detect conflicts and replace using the
    // existing casing without per-header array copies + toLowerCase scans.
    let existing: Map<string, string> | undefined;
    let contentType: string | undefined;
    if (raw.length > 0) {
      existing = new Map();
      for (const [n, v] of raw) {
        const lower = n.toLowerCase();
        existing.set(lower, n);
        if (lower === 'content-type') contentType = v;
      }
    }

    const nonce = (ctx as unknown as Record<string, unknown>)[CSP_NONCE_CONTEXT_KEY];
    const hasNonce = typeof nonce === 'string' && nonce.length > 0;

    for (const h of isJsonContentType(contentType) ? jsonSet : precomputed) {
      // HSTS is ignored by browsers over plaintext and is surprising in local /
      // HTTP-terminated setups — only emit it when the request is secure.
      if (h.httpsOnly && !ctx.secured()) continue;
      const presentName = existing?.get(h.lowerName);
      // Preserve a header an inner middleware set, unless this one was set explicitly.
      if (presentName !== undefined && !h.forced) continue;
      const value = h.isCsp && hasNonce ? withCspNonce(h.value, nonce as string) : h.value;
      res = res.setHeader(presentName ?? h.name, value);
    }

    return res;
  };

  // No async layer of its own: the headers apply in the continuation of the
  // inner chain's promise.
  const middleware: HttpMiddleware = (ctx, next) => {
    // Per-route opt-out: skip only when every matched candidate opts out.
    if (skipsSecurityHeaders(ctx)) return next();

    let inner: Promise<HttpResponse | undefined>;
    try {
      inner = next();
    } catch (error) {
      return Promise.reject(error);
    }
    const apply = (res: HttpResponse | undefined) => secure(ctx, res);
    // A middleware that broke its contract and returned a plain value still works.
    return inner instanceof Promise ? inner.then(apply) : Promise.resolve(inner).then(apply);
  };
  // A route that opted out of the headers, or a minimal one, never runs it.
  return actsOnRoutes(middleware, (route) => !route.minimal && route.securityHeaders);
};
