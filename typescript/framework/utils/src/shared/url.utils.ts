/**
 * URL Utility Functions
 *
 * Browser and SSR compatible URL manipulation utilities.
 * @module @putnami/utils
 */

/**
 * Builds a URL with query parameters and path parameter substitution.
 *
 * Supports both path parameters (`[paramName]`) and query parameters:
 * - Path parameters are substituted directly in the URL path
 * - Other parameters are appended as query string parameters
 *
 * @param baseUrl - The base URL, optionally containing path parameters like `[id]`
 * @param params - Object of parameters to substitute/append
 * @returns The fully constructed URL with parameters
 *
 * @example
 * ```typescript
 * // Query parameters
 * buildUrlWithParams('https://api.example.com/users', { page: 1, limit: 10 });
 * // 'https://api.example.com/users?page=1&limit=10'
 *
 * // Path parameters
 * buildUrlWithParams('https://api.example.com/users/[id]', { id: '123' });
 * // 'https://api.example.com/users/123'
 *
 * // Mixed
 * buildUrlWithParams('https://api.example.com/users/[id]/posts', { id: '123', page: 1 });
 * // 'https://api.example.com/users/123/posts?page=1'
 * ```
 */
export function buildUrlWithParams(baseUrl: string, params?: Record<string, unknown>): string {
  if (!params) {
    return baseUrl;
  }

  let url = baseUrl;
  const queryParts: string[] = [];

  for (const [param, value] of Object.entries(params)) {
    if (value == null) {
      continue;
    }

    const token = `[${param}]`;
    if (url.includes(token)) {
      // Encode path-parameter values, matching the query branch below. Raw
      // substitution would let a value containing `/`, `?`, `#`, `%`, or `..`
      // break out of its path segment (traversal / request smuggling / broken URLs).
      url = url.replace(token, encodeURIComponent(String(value)));
      continue;
    }

    queryParts.push(`${param}=${encodeURIComponent(String(value))}`);
  }

  if (!queryParts.length) {
    return url;
  }

  const needsSeparator = !url.endsWith('?') && !url.endsWith('&');
  const separator = url.includes('?') ? (needsSeparator ? '&' : '') : '?';
  return `${url}${separator}${queryParts.join('&')}`;
}

/**
 * Parses a query string into an object of key-value pairs.
 *
 * @param query - The query string to parse (with or without leading `?`)
 * @returns An object with the parsed query parameters
 *
 * @example
 * ```typescript
 * parseQueryString('page=1&limit=10');
 * // { page: '1', limit: '10' }
 *
 * parseQueryString('?search=hello%20world');
 * // { search: 'hello world' }
 *
 * parseQueryString('');
 * // {}
 * ```
 */
export function parseQueryString(query: string): Record<string, string> {
  return Object.fromEntries(new URLSearchParams(query));
}

/**
 * Extracts the base URL (origin + pathname) from a full URL, removing query and hash.
 *
 * @param url - The full URL
 * @returns The base URL without query string or hash
 *
 * @example
 * ```typescript
 * getBaseUrl('https://example.com/path?query=1#hash');
 * // 'https://example.com/path'
 *
 * getBaseUrl('https://example.com');
 * // 'https://example.com'
 * ```
 */
export function getBaseUrl(url: string): string {
  try {
    const parsed = new URL(url);
    return `${parsed.origin}${parsed.pathname}`;
  } catch {
    // If URL parsing fails, try to strip query/hash manually
    return url.split('?')[0].split('#')[0];
  }
}

/**
 * Joins URL path segments, ensuring proper slash handling.
 *
 * @param segments - Path segments to join
 * @returns The joined URL path
 *
 * @example
 * ```typescript
 * joinUrlPath('https://api.example.com', 'users', '123');
 * // 'https://api.example.com/users/123'
 *
 * joinUrlPath('/api/', '/users/', '/list');
 * // '/api/users/list'
 * ```
 */
export function joinUrlPath(...segments: string[]): string {
  return segments
    .map((segment, index) => {
      if (index === 0) {
        return segment.replace(/\/+$/, '');
      }
      return segment.replace(/^\/+|\/+$/g, '');
    })
    .filter(Boolean)
    .join('/');
}
