import {
  MAX_PATH_LEN,
  MAX_REFERRER_LEN,
  MAX_UTM_LEN,
  type ReferrerType,
  utf8Bytes,
  UTM_KEYS,
  type UtmFields,
} from '../sanitize/vocabulary';

export type { ReferrerType, UtmFields } from '../sanitize/vocabulary';

/** The counter key used when a dimension has no value. */
export const NO_HOST = '__none__';

/** Where a visit came from, in the closed vocabulary of the protocol. */
export interface ReferrerInfo {
  /** The stored referrer: `origin + pathname`, or an app-relative path, or null. */
  referrer: string | null;
  /** The classification of {@link ReferrerInfo.referrer}. */
  referrerType: ReferrerType;
  /** The counter key: the hostname without `www.`, or `__none__`. */
  referrerHost: string;
}

/**
 * Search engines. An entry ending with `.` is a prefix match on the host, so
 * `google.` covers every country domain; any other entry matches the host
 * exactly or as a `*.` suffix.
 */
const SEARCH_HOSTS: readonly string[] = [
  'google.',
  'bing.com',
  'duckduckgo.com',
  'yahoo.',
  'yandex.',
  'baidu.com',
  'ecosia.org',
  'qwant.com',
];

/** Social networks, matched the same way as {@link SEARCH_HOSTS}. */
const SOCIAL_HOSTS: readonly string[] = [
  'facebook.com',
  'instagram.com',
  'twitter.com',
  'x.com',
  't.co',
  'linkedin.com',
  'lnkd.in',
  'reddit.com',
  'pinterest.',
  'tiktok.com',
  'youtube.com',
  'youtu.be',
  'threads.net',
  'mastodon.social',
  'bsky.app',
];

/**
 * Classifies a referrer against the request's own host.
 *
 * The query string and the fragment are dropped before anything is stored:
 * a search referrer carries the visitor's query terms, which are personal data
 * this package never persists. A referrer that does not parse is treated as
 * absent rather than as a value, because a browser-supplied string is not an
 * oracle for anything.
 *
 * @param raw - The `Referer` header or the wire `page.referrer`.
 * @param requestHost - The host the request arrived on (`ctx.host()`).
 * @returns The stored referrer, its type, and the counter key.
 */
export function classifyReferrer(raw: string | null | undefined, requestHost: string): ReferrerInfo {
  const value = raw?.trim() ?? '';
  if (value === '') {
    return { referrer: null, referrerType: 'direct', referrerHost: NO_HOST };
  }
  const own = normalizeHost(requestHost);
  if (isAppRelative(value)) {
    return {
      referrer: truncateUtf8(value, MAX_REFERRER_LEN),
      referrerType: 'internal',
      referrerHost: own === '' ? NO_HOST : own,
    };
  }

  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return { referrer: null, referrerType: 'direct', referrerHost: NO_HOST };
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') {
    return { referrer: null, referrerType: 'direct', referrerHost: NO_HOST };
  }
  const host = normalizeHost(url.host);
  return {
    referrer: truncateUtf8(url.origin + url.pathname, MAX_REFERRER_LEN),
    referrerType: classifyHost(host, own),
    referrerHost: host === '' ? NO_HOST : host,
  };
}

/**
 * Reads the five campaign parameters from a landing-page query string.
 *
 * @param query - The parsed query string, or a plain record of its parameters.
 * @returns The campaign values, trimmed and bounded; absent keys stay absent.
 */
export function extractUtm(query: URLSearchParams | Record<string, string> | undefined): UtmFields {
  if (query === undefined) {
    return {};
  }
  const utm: UtmFields = {};
  for (const key of UTM_KEYS) {
    const name = `utm_${key}`;
    const raw = query instanceof URLSearchParams ? query.get(name) : query[name];
    const value = raw?.trim() ?? '';
    if (value !== '') {
      utm[key] = truncateUtf8(value, MAX_UTM_LEN);
    }
  }
  return utm;
}

/**
 * Folds a server-derived URL into the stored `path`.
 *
 * This one truncates instead of rejecting: the value comes from the request
 * line the server itself matched, so there is no client to send the rejection
 * to. A wire `page.path` beyond the bound is rejected by the sanitizer instead.
 *
 * The result always starts with `/`. `ctx.path()` reports bare segments
 * (`tasks`, not `/tasks`) while the wire contract requires the leading slash,
 * so without it the same page would key two `path` counters — one for the
 * server row and one for the browser's.
 *
 * @param raw - The request path, possibly with a query or a fragment.
 * @returns A bounded, root-relative path.
 */
export function normalizePath(raw: string): string {
  const cut = raw.search(/[?#]/);
  const path = cut === -1 ? raw : raw.slice(0, cut);
  const rooted = path.startsWith('/') ? path : `/${path}`;
  return rooted === '/' ? '/' : truncateUtf8(rooted, MAX_PATH_LEN);
}

/**
 * Truncates a string to a UTF-8 byte bound without splitting a character.
 *
 * Slicing by `length` would count UTF-16 units and could emit a lone
 * surrogate, so the fold walks code points and stops before the bound.
 *
 * @param value - The string to bound.
 * @param maxBytes - The inclusive byte ceiling.
 * @returns The longest prefix of `value` that fits in `maxBytes` bytes.
 */
export function truncateUtf8(value: string, maxBytes: number): string {
  if (utf8Bytes(value) <= maxBytes) {
    return value;
  }
  let bytes = 0;
  let out = '';
  for (const character of value) {
    const size = utf8Bytes(character);
    if (bytes + size > maxBytes) {
      break;
    }
    bytes += size;
    out += character;
  }
  return out;
}

/**
 * Reports whether a referrer is an app-relative path.
 *
 * `//host/path` and `/\host/path` are not: a browser resolves both against the
 * current scheme, so accepting them would file another origin as internal.
 */
function isAppRelative(value: string): boolean {
  if (!value.startsWith('/')) {
    return false;
  }
  return value.length < 2 || (value[1] !== '/' && value[1] !== '\\');
}

/** Lowercases a host and drops a leading `www.`, on both sides of a compare. */
function normalizeHost(host: string): string {
  const lowered = host.toLowerCase().trim();
  return lowered.startsWith('www.') ? lowered.slice(4) : lowered;
}

function classifyHost(host: string, own: string): ReferrerType {
  if (host !== '' && host === own) {
    return 'internal';
  }
  if (SEARCH_HOSTS.some((entry) => matchesHost(host, entry))) {
    return 'search';
  }
  if (SOCIAL_HOSTS.some((entry) => matchesHost(host, entry))) {
    return 'social';
  }
  return 'other';
}

/** Prefix match for a trailing-dot entry, exact or `*.` suffix otherwise. */
function matchesHost(host: string, entry: string): boolean {
  if (entry.endsWith('.')) {
    return host.startsWith(entry);
  }
  return host === entry || host.endsWith(`.${entry}`);
}
