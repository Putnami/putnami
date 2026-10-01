import { describe, expect, it } from 'bun:test';
import { buildTrustedProxyMatcher, parseIpAddress } from '../../src/http/ip-prefix';

describe('parseIpAddress', () => {
  it('parses IPv4 into 4 canonical bytes', () => {
    expect(Array.from(parseIpAddress('192.168.1.9') ?? [])).toEqual([192, 168, 1, 9]);
  });

  it('parses IPv6 into 16 canonical bytes', () => {
    expect(parseIpAddress('2001:db8::1')?.length).toBe(16);
    expect(parseIpAddress('::1')?.length).toBe(16);
  });

  it('canonicalizes IPv4-mapped IPv6 to its 4-byte IPv4 form (Go Unmap parity)', () => {
    // ::ffff:1.2.3.4 collapses to the IPv4 form so it compares equal to 1.2.3.4.
    expect(Array.from(parseIpAddress('::ffff:1.2.3.4') ?? [])).toEqual([1, 2, 3, 4]);
  });

  it('keeps IPv4-compatible (::a.b.c.d, no ffff) as IPv6, matching Go Is4In6=false', () => {
    // ::1.2.3.4 is NOT IPv4-mapped in Go; it stays a 16-byte IPv6 address.
    expect(parseIpAddress('::1.2.3.4')?.length).toBe(16);
  });

  it('parses embedded IPv4 in an IPv6 tail', () => {
    const bytes = parseIpAddress('2001:db8::1.2.3.4');
    expect(bytes?.length).toBe(16);
    expect(Array.from(bytes?.slice(12) ?? [])).toEqual([1, 2, 3, 4]);
  });

  it('returns undefined for malformed input instead of throwing', () => {
    for (const bad of [
      '',
      'not-an-ip',
      '010.0.0.1', // leading zero
      '1.2.3.256', // octet out of range
      '1.2.3', // too few octets
      '1.2.3.4.5', // too many octets
      '1:2:3:4:5:6:7:8:9', // too many hextets
      '1::2::3', // double compression
      'gg::1', // bad hex
      'fe80::1%eth0', // zone
      ':1', // stray leading colon
      '12345::', // hextet too long
    ]) {
      expect(parseIpAddress(bad)).toBeUndefined();
    }
  });
});

describe('buildTrustedProxyMatcher', () => {
  it('matches an exact IPv4 entry', () => {
    const m = buildTrustedProxyMatcher(['10.0.0.1']);
    expect(m.matches('10.0.0.1')).toBe(true);
    expect(m.matches('10.0.0.2')).toBe(false);
  });

  it('matches an exact IPv6 entry', () => {
    const m = buildTrustedProxyMatcher(['::1']);
    expect(m.matches('::1')).toBe(true);
    expect(m.matches('::2')).toBe(false);
  });

  it('matches inside an IPv4 CIDR and rejects outside', () => {
    const m = buildTrustedProxyMatcher(['10.0.0.0/8']);
    expect(m.matches('10.4.5.6')).toBe(true);
    expect(m.matches('172.16.0.1')).toBe(false);
  });

  it('matches inside an IPv6 CIDR and rejects outside', () => {
    const m = buildTrustedProxyMatcher(['2001:db8::/32']);
    expect(m.matches('2001:db8::1')).toBe(true);
    expect(m.matches('2001:db9::1')).toBe(false);
  });

  it('matches an IPv4-mapped IPv6 peer against an IPv4 CIDR (adversarial edge)', () => {
    const m = buildTrustedProxyMatcher(['10.0.0.0/8']);
    expect(m.matches('::ffff:10.4.5.6')).toBe(true);
    // ...and the exact-entry counterpart compares equal too.
    expect(buildTrustedProxyMatcher(['10.0.0.1']).matches('::ffff:10.0.0.1')).toBe(true);
  });

  it('never matches an IPv4 peer against an IPv6 CIDR (family gate)', () => {
    expect(buildTrustedProxyMatcher(['2001:db8::/32']).matches('10.0.0.1')).toBe(false);
    expect(buildTrustedProxyMatcher(['10.0.0.0/8']).matches('2001:db8::1')).toBe(false);
  });

  it('respects /24 boundaries', () => {
    const m = buildTrustedProxyMatcher(['192.168.1.0/24']);
    expect(m.matches('192.168.1.0')).toBe(true);
    expect(m.matches('192.168.1.255')).toBe(true);
    expect(m.matches('192.168.2.0')).toBe(false);
    expect(m.matches('192.168.0.255')).toBe(false);
  });

  it('respects /64 boundaries', () => {
    const m = buildTrustedProxyMatcher(['2001:db8:0:1::/64']);
    expect(m.matches('2001:db8:0:1::')).toBe(true);
    expect(m.matches('2001:db8:0:1:ffff:ffff:ffff:ffff')).toBe(true);
    expect(m.matches('2001:db8:0:2::')).toBe(false);
    expect(m.matches('2001:db8::')).toBe(false);
  });

  it('skips blank and malformed entries without disabling the whole list', () => {
    const m = buildTrustedProxyMatcher(['', '   ', 'garbage', '999.999.999.999', '10.0.0.0/99', '10.0.0.0/8']);
    // Only the valid /8 survives.
    expect(m.size).toBe(1);
    expect(m.matches('10.1.2.3')).toBe(true);
    expect(m.matches('11.0.0.1')).toBe(false);
  });

  it('trusts nothing when the list is empty', () => {
    const m = buildTrustedProxyMatcher([]);
    expect(m.size).toBe(0);
    expect(m.matches('10.0.0.1')).toBe(false);
  });

  it('never matches undefined or empty peer addresses', () => {
    const m = buildTrustedProxyMatcher(['10.0.0.0/8']);
    expect(m.matches(undefined)).toBe(false);
    expect(m.matches('')).toBe(false);
    expect(m.matches('not-an-ip')).toBe(false);
  });
});
