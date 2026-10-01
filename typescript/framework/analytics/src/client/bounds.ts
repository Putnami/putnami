/** Byte bound of `page.path` (protocol `MaxPathLen`). */
export const MAX_PATH_LEN = 512;
/** Byte bound of `page.route` (protocol `MaxRouteLen`). */
export const MAX_ROUTE_LEN = 256;
/** Byte bound of `page.referrer` (protocol `MaxReferrerLen`). */
export const MAX_REFERRER_LEN = 512;
/** Byte bound of each `page.utm` value (protocol `MaxUTMLen`). */
export const MAX_UTM_LEN = 128;
/** Byte bound of a string-valued action property (protocol `MaxPropStringLen`). */
export const MAX_PROP_STRING_LEN = 256;
/** The largest number of action properties (protocol `MaxPropKeys`). */
export const MAX_PROP_KEYS = 20;
/** Action property key (protocol `PropKeyRe`). */
export const PROP_KEY_RE = /^[a-z][a-z0-9_]{0,31}$/;

/** Bytes one code point occupies when UTF-8 encoded. */
function codePointBytes(codePoint: number): number {
  if (codePoint < 0x80) {
    return 1;
  }
  if (codePoint < 0x8_00) {
    return 2;
  }
  return codePoint < 0x1_00_00 ? 3 : 4;
}

/**
 * Length of a value in UTF-8 bytes.
 *
 * Every bound in the contract is a byte bound, because the Go validator
 * applies `len()` to a string. `value.length` counts UTF-16 code units, so a
 * 200-character Cyrillic campaign name measures 200 there and 400 on the wire
 * — a tracker using it would emit events the server rejects, and no ASCII
 * fixture could reveal it. Hand-rolled rather than `TextEncoder` because the
 * encoder allocates a buffer per call and this runs on every event.
 *
 * @param value - The string to measure.
 * @returns The number of bytes the string occupies when UTF-8 encoded.
 */
export function utf8Bytes(value: string): number {
  let bytes = 0;
  for (const character of value) {
    bytes += codePointBytes(character.codePointAt(0) ?? 0);
  }
  return bytes;
}

/**
 * Truncates a value to a byte bound, never splitting a code point.
 *
 * Truncating client-side keeps the *event*: the sanitizer rejects an
 * over-long value outright, so an untruncated campaign name would take the
 * whole page view down with it.
 *
 * @param value - The string to bound.
 * @param maxBytes - The byte ceiling.
 * @returns The longest prefix of `value` that fits.
 */
export function bounded(value: string, maxBytes: number): string {
  if (utf8Bytes(value) <= maxBytes) {
    return value;
  }
  let out = '';
  let bytes = 0;
  for (const character of value) {
    const size = codePointBytes(character.codePointAt(0) ?? 0);
    if (bytes + size > maxBytes) {
      break;
    }
    out += character;
    bytes += size;
  }
  return out;
}
