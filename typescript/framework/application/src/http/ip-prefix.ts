/**
 * Hand-rolled IPv4 / IPv6 address and CIDR-prefix matching for trusted-proxy
 * resolution.
 *
 * No external dependencies (repo rule) and — critically — no throw on hostile
 * input: every parser returns `undefined` for malformed data so the request-key
 * path can treat a parse failure as "not trusted" / "use the peer", never a 500.
 *
 * The semantics mirror Go's `net/netip` as used by
 * `go/framework/http/middleware_ratelimit.go` (`buildRateLimitKeyFunc`):
 *   - exact IPs and CIDR prefixes are compared by masked-byte equality;
 *   - families must match — an IPv4 address never matches an IPv6 prefix, the
 *     same way Go's `Prefix.Contains` gates on `BitLen`;
 *   - IPv4-mapped IPv6 addresses (`::ffff:a.b.c.d`) are canonicalized to their
 *     4-byte IPv4 form (Go's `Addr.Unmap`) before comparison. Go's net stack
 *     normalizes `RemoteAddr` to IPv4 for IPv4 clients, so an IPv4 peer that a
 *     dual-stack socket surfaces as `::ffff:…` still matches an IPv4 CIDR — this
 *     canonicalization reproduces that effective behavior.
 */

const IPV4_BYTES = 4;
const IPV6_BYTES = 16;
const IPV6_GROUPS = 8;

/**
 * Parse an IPv4 or IPv6 address into canonical bytes (4 bytes for IPv4, 16 for
 * IPv6, 4 for an IPv4-mapped IPv6 address). Returns `undefined` for malformed
 * input; never throws.
 */
export function parseIpAddress(input: string): Uint8Array | undefined {
  if (input.length === 0) return undefined;
  // Every IPv6 form contains a colon; IPv4 never does. An IPv4-mapped or
  // embedded-v4 IPv6 form (`::ffff:1.2.3.4`) carries both `:` and `.` and is
  // handled by the v6 parser.
  if (input.indexOf(':') !== -1) return parseIpv6(input);
  return parseIpv4(input);
}

function parseIpv4(input: string): Uint8Array | undefined {
  const parts = input.split('.');
  if (parts.length !== IPV4_BYTES) return undefined;
  const bytes = new Uint8Array(IPV4_BYTES);
  for (let i = 0; i < IPV4_BYTES; i++) {
    const octet = parseOctet(parts[i]);
    if (octet === undefined) return undefined;
    bytes[i] = octet;
  }
  return bytes;
}

function parseOctet(segment: string): number | undefined {
  const len = segment.length;
  if (len < 1 || len > 3) return undefined;
  // Reject leading zeros ("010") to match Go's netip, which forbids them.
  if (len > 1 && segment.charCodeAt(0) === 0x30) return undefined;
  let value = 0;
  for (let i = 0; i < len; i++) {
    const code = segment.charCodeAt(i);
    if (code < 0x30 || code > 0x39) return undefined;
    value = value * 10 + (code - 0x30);
  }
  return value > 255 ? undefined : value;
}

function parseIpv6(input: string): Uint8Array | undefined {
  // Zones (`fe80::1%eth0`) are never honored for keying: Go's `Prefix.Contains`
  // rejects zoned addresses, and an exact match on a zoned form is meaningless
  // for a proxy allowlist. Treat as malformed → no match.
  if (input.indexOf('%') !== -1) return undefined;

  let text = input;

  // An embedded IPv4 tail (`::ffff:1.2.3.4`, `2001:db8::1.2.3.4`) occupies the
  // final 32 bits. Rewrite it into two hextets so the rest parses uniformly.
  const lastColon = text.lastIndexOf(':');
  if (lastColon !== -1 && text.indexOf('.', lastColon + 1) !== -1) {
    const v4 = parseIpv4(text.slice(lastColon + 1));
    if (v4 === undefined) return undefined;
    const hi = ((v4[0] << 8) | v4[1]).toString(16);
    const lo = ((v4[2] << 8) | v4[3]).toString(16);
    text = `${text.slice(0, lastColon + 1)}${hi}:${lo}`;
  }

  // At most one `::` compression is allowed.
  const halves = text.split('::');
  if (halves.length > 2) return undefined;

  const groups: number[] = [];
  if (halves.length === 2) {
    const head = parseHextets(halves[0]);
    const tail = parseHextets(halves[1]);
    if (head === undefined || tail === undefined) return undefined;
    const missing = IPV6_GROUPS - head.length - tail.length;
    // `::` must stand for at least one group of zeros.
    if (missing < 1) return undefined;
    for (const group of head) groups.push(group);
    for (let i = 0; i < missing; i++) groups.push(0);
    for (const group of tail) groups.push(group);
  } else {
    const all = parseHextets(halves[0]);
    if (all === undefined || all.length !== IPV6_GROUPS) return undefined;
    for (const group of all) groups.push(group);
  }
  if (groups.length !== IPV6_GROUPS) return undefined;

  const bytes = new Uint8Array(IPV6_BYTES);
  for (let i = 0; i < IPV6_GROUPS; i++) {
    bytes[i * 2] = (groups[i] >> 8) & 0xff;
    bytes[i * 2 + 1] = groups[i] & 0xff;
  }

  // IPv4-mapped (`::ffff:a.b.c.d`) collapses to its canonical 4-byte form,
  // matching Go's `Addr.Unmap`. IPv4-compatible (`::a.b.c.d`, no `ffff`) stays
  // IPv6, exactly as Go's `Is4In6` reports it.
  if (isIpv4Mapped(bytes)) return bytes.slice(12, 16);
  return bytes;
}

function parseHextets(segment: string): number[] | undefined {
  if (segment.length === 0) return [];
  const parts = segment.split(':');
  const out: number[] = [];
  for (const part of parts) {
    const value = parseHextet(part);
    if (value === undefined) return undefined;
    out.push(value);
  }
  return out;
}

function parseHextet(segment: string): number | undefined {
  const len = segment.length;
  if (len < 1 || len > 4) return undefined;
  let value = 0;
  for (let i = 0; i < len; i++) {
    const digit = hexDigit(segment.charCodeAt(i));
    if (digit === undefined) return undefined;
    value = value * 16 + digit;
  }
  return value;
}

function hexDigit(code: number): number | undefined {
  if (code >= 0x30 && code <= 0x39) return code - 0x30; // 0-9
  if (code >= 0x61 && code <= 0x66) return code - 0x61 + 10; // a-f
  if (code >= 0x41 && code <= 0x46) return code - 0x41 + 10; // A-F
  return undefined;
}

function isIpv4Mapped(bytes: Uint8Array): boolean {
  for (let i = 0; i < 10; i++) {
    if (bytes[i] !== 0) return false;
  }
  return bytes[10] === 0xff && bytes[11] === 0xff;
}

/** A parsed CIDR prefix: the masked network address plus its prefix length. */
interface IpPrefix {
  bytes: Uint8Array;
  bits: number;
}

function parseIpPrefix(input: string): IpPrefix | undefined {
  const slash = input.indexOf('/');
  if (slash === -1) return undefined;
  const addr = parseIpAddress(input.slice(0, slash));
  if (addr === undefined) return undefined;
  const bits = parseBits(input.slice(slash + 1));
  if (bits === undefined || bits > addr.length * 8) return undefined;
  return { bytes: maskAddress(addr, bits), bits };
}

function parseBits(segment: string): number | undefined {
  const len = segment.length;
  if (len < 1 || len > 3) return undefined;
  let value = 0;
  for (let i = 0; i < len; i++) {
    const code = segment.charCodeAt(i);
    if (code < 0x30 || code > 0x39) return undefined;
    value = value * 10 + (code - 0x30);
  }
  return value;
}

function maskAddress(addr: Uint8Array, bits: number): Uint8Array {
  const out = new Uint8Array(addr.length);
  for (let i = 0; i < addr.length; i++) {
    const remaining = bits - i * 8;
    if (remaining >= 8) {
      out[i] = addr[i];
    } else if (remaining <= 0) {
      out[i] = 0;
    } else {
      const mask = (0xff << (8 - remaining)) & 0xff;
      out[i] = addr[i] & mask;
    }
  }
  return out;
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

function matchesPrefix(addr: Uint8Array, prefix: IpPrefix): boolean {
  // Families must match (4 bytes vs 16), reproducing Go's BitLen gate.
  if (addr.length !== prefix.bytes.length) return false;
  return bytesEqual(maskAddress(addr, prefix.bits), prefix.bytes);
}

/** Matches a peer address against a set of trusted exact IPs and CIDR ranges. */
export interface TrustedProxyMatcher {
  /** Count of parseable entries. Zero ⇒ nothing is ever trusted. */
  readonly size: number;
  /** True when `ip` (a bare peer address) matches a trusted exact IP or CIDR. */
  matches(ip: string | undefined): boolean;
}

/**
 * Build a {@link TrustedProxyMatcher} from raw config entries. Each entry is an
 * exact IP (`10.0.0.1`, `::1`) or a CIDR range (`10.0.0.0/8`, `2001:db8::/32`).
 *
 * Blank and malformed entries are skipped individually — a single bad entry
 * never throws and never disables the rest of the list — mirroring Go's
 * `buildRateLimitKeyFunc`, which silently drops entries `netip` cannot parse.
 */
export function buildTrustedProxyMatcher(entries: readonly string[]): TrustedProxyMatcher {
  const exact: Uint8Array[] = [];
  const prefixes: IpPrefix[] = [];
  for (const raw of entries) {
    const entry = raw.trim();
    if (entry.length === 0) continue;
    if (entry.indexOf('/') !== -1) {
      const prefix = parseIpPrefix(entry);
      if (prefix !== undefined) prefixes.push(prefix);
      continue;
    }
    const addr = parseIpAddress(entry);
    if (addr !== undefined) exact.push(addr);
  }

  const size = exact.length + prefixes.length;
  return {
    size,
    matches(ip: string | undefined): boolean {
      if (size === 0 || ip === undefined || ip.length === 0) return false;
      const addr = parseIpAddress(ip);
      if (addr === undefined) return false;
      for (const candidate of exact) {
        if (bytesEqual(candidate, addr)) return true;
      }
      for (const prefix of prefixes) {
        if (matchesPrefix(addr, prefix)) return true;
      }
      return false;
    },
  };
}
