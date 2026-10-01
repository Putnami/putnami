/**
 * Content Security Policy (CSP) nonce utilities for SSR.
 *
 * Generates a cryptographic nonce per request and provides helpers
 * to build CSP directives that allow the framework's inline scripts
 * (hydration data) while blocking unauthorized scripts.
 *
 * The SSR page renderer applies nonce attributes automatically and publishes
 * the nonce on the request context. The application security header middleware
 * uses that value when CSP headers are enabled. The helpers below are exported
 * for callers that need to build a custom policy or reuse a nonce.
 *
 * @example
 * ```ts
 * import { generateCspNonce, buildCspHeader } from '@putnami/web';
 *
 * // To build a custom policy (e.g. allowing a CDN) in a middleware:
 * const nonce = generateCspNonce();
 * const csp = buildCspHeader({ nonce, scriptSrc: ['https://cdn.example.com'] });
 * // → "default-src 'self'; script-src 'self' 'nonce-<base64>' https://cdn.example.com; ..."
 * ```
 */

import { randomBytes } from 'node:crypto';

/**
 * Generate a cryptographically random nonce for CSP script-src directives.
 * Returns a 16-byte base64-encoded string suitable for use in `nonce-<value>` attributes.
 */
export function generateCspNonce(): string {
  return randomBytes(16).toString('base64');
}

/** Options for building a CSP header string. */
export interface CspOptions {
  /** Nonce to allow inline scripts. Added as `'nonce-<value>'` to script-src. */
  nonce?: string;
  /** Additional script-src values (e.g., CDN domains). */
  scriptSrc?: string[];
  /** Additional style-src values. Default includes `'unsafe-inline'` for Emotion. */
  styleSrc?: string[];
  /** Additional connect-src values (e.g., API domains, WebSocket endpoints). */
  connectSrc?: string[];
  /** Additional img-src values. */
  imgSrc?: string[];
  /** Additional font-src values. */
  fontSrc?: string[];
  /** Override or extend any directive with raw directive strings. */
  directives?: Record<string, string>;
}

/**
 * Build a Content-Security-Policy header string with sensible defaults for SSR apps.
 *
 * The defaults follow the `@putnami/application` SecurityHeadersMiddleware pattern
 * but add nonce support for inline scripts used by the SSR hydration layer.
 *
 * @param options - CSP configuration options
 * @returns A complete CSP header string
 */
export function buildCspHeader(options: CspOptions = {}): string {
  const { nonce, scriptSrc = [], styleSrc = [], connectSrc = [], imgSrc = [], fontSrc = [], directives = {} } = options;

  const nonceDirective = nonce ? `'nonce-${nonce}'` : '';

  const parts: Record<string, string> = {
    'default-src': "'self'",
    'script-src': ["'self'", nonceDirective, ...scriptSrc].filter(Boolean).join(' '),
    'style-src': ["'self'", "'unsafe-inline'", ...styleSrc].filter(Boolean).join(' '),
    'img-src': ["'self'", 'data:', ...imgSrc].filter(Boolean).join(' '),
    'font-src': ["'self'", ...fontSrc].filter(Boolean).join(' '),
    'connect-src': ["'self'", ...connectSrc].filter(Boolean).join(' '),
    'frame-ancestors': "'none'",
    'base-uri': "'self'",
    'form-action': "'self'",
    ...directives,
  };

  return Object.entries(parts)
    .map(([key, value]) => `${key} ${value}`)
    .join('; ');
}
