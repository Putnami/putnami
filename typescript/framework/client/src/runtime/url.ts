/**
 * URL scheme allow-listing for client transports.
 *
 * The primary request URL (`baseUrl`) flows into `fetch` and `WebSocket`. If it
 * carries a non-http scheme (`file:`, `ftp:`, cloud-metadata gopher tricks, …)
 * it becomes an SSRF / local-file-read vector. Every entry point that accepts a
 * base URL runs it through {@link assertHttpUrl} so only `http:`/`https:` reach
 * the network layer.
 */

/**
 * Validate that a URL uses an allowed scheme (http or https) and return it
 * unchanged. Throws a descriptive error otherwise.
 *
 * @param url - the URL to validate
 * @param source - human-readable label for the URL's origin, used in errors
 */
export function assertHttpUrl(url: string, source = 'baseUrl'): string {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    throw new Error(`Invalid ${source}: "${url}" is not a valid URL`);
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new Error(
      `Invalid ${source} scheme: only http:// and https:// are allowed, got "${parsed.protocol}" in "${url}"`,
    );
  }
  return url;
}

interface ServiceUrlOptions {
  source?: string;
  allowInsecure?: boolean;
}

/** Build a transport URL from provider-authored path and schema-encoded parameters. */
export function buildRequestUrl(
  baseUrl: string,
  request: {
    path: string;
    params?: Record<string, string>;
    query?: Record<string, string | readonly string[] | undefined>;
  },
): string {
  let path = request.path;
  for (const [key, value] of Object.entries(request.params ?? {})) {
    path = path.replace(`{${key}}`, encodeURIComponent(value));
  }
  const unreplaced = path.match(/\{[^}]+\}/g);
  if (unreplaced) throw new Error(`Missing path parameters: ${unreplaced.join(', ')} in "${request.path}"`);

  const url = new URL(`${baseUrl}${path}`);
  for (const [key, value] of Object.entries(request.query ?? {})) {
    if (value === undefined) continue;
    if (typeof value === 'string') url.searchParams.append(key, value);
    else for (const item of value) url.searchParams.append(key, item);
  }
  return url.toString();
}

/**
 * Validate and canonicalize a first-party service or credential endpoint.
 * Authority-changing URL components are rejected because generated clients
 * attach credentials to this authority without following redirects.
 */
export function parseServiceUrl(url: string, options: ServiceUrlOptions = {}): string {
  const source = options.source ?? 'service URL';
  let parsed: URL;
  try {
    parsed = new URL(url.trim());
  } catch {
    throw new Error(`Invalid ${source}: expected an absolute HTTP URL`);
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new Error(`Invalid ${source}: expected an HTTP or HTTPS URL`);
  }
  if (parsed.username || parsed.password || parsed.search || parsed.hash) {
    throw new Error(`Invalid ${source}: user info, query, and fragment are forbidden`);
  }
  const loopback = isLoopbackHost(parsed.hostname);
  if (parsed.protocol !== 'https:' && !loopback && !options.allowInsecure) {
    throw new Error(`Invalid ${source}: HTTPS is required for non-loopback services`);
  }
  parsed.pathname = parsed.pathname.replace(/\/+$/, '');
  return parsed.toString().replace(/\/$/, '');
}

function isLoopbackHost(hostname: string): boolean {
  if (hostname === 'localhost' || hostname === '[::1]') return true;
  const ipv4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(hostname);
  if (ipv4) {
    const octets = ipv4.slice(1).map(Number);
    return octets.every((octet) => octet >= 0 && octet <= 255) && octets[0] === 127;
  }
  const mapped = /^\[::ffff:(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})]$/i.exec(hostname);
  if (!mapped) return false;
  const octets = mapped.slice(1).map(Number);
  return octets.every((octet) => octet >= 0 && octet <= 255) && octets[0] === 127;
}

/**
 * Validate that a URL is usable as a WebSocket base URL and return it unchanged.
 *
 * The WebSocket transport accepts either `http(s)` (which it converts to
 * `ws(s)`) or an already-`ws(s)` URL. Everything else (`file:`, `ftp:`, …) is
 * rejected for the same SSRF reasons as {@link assertHttpUrl}.
 */
export function assertWebSocketUrl(url: string, source = 'baseUrl'): string {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    throw new Error(`Invalid ${source}: "${url}" is not a valid URL`);
  }
  const allowed = ['http:', 'https:', 'ws:', 'wss:'];
  if (!allowed.includes(parsed.protocol)) {
    throw new Error(
      `Invalid ${source} scheme: only http(s):// and ws(s):// are allowed, got "${parsed.protocol}" in "${url}"`,
    );
  }
  return url;
}
