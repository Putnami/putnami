import { describe, expect, it } from 'bun:test';
import type { HttpRequestContext } from '@putnami/application';
import { createClientIpResolver } from '../src/server/identity/client-ip';

/** A request context carrying a socket peer and the headers a proxy would add. */
function context(peer: string | undefined, headers: Record<string, string> = {}): HttpRequestContext {
  const req = new Request('http://localhost/_putnami/analytics/events');
  return {
    req,
    headers: new Headers(headers),
    server: { requestIP: () => (peer === undefined ? undefined : { address: peer }) },
  } as unknown as HttpRequestContext;
}

describe('createClientIpResolver', () => {
  it('ignores X-Forwarded-For when no proxy is trusted', () => {
    const resolve = createClientIpResolver(undefined);
    const ctx = context('198.51.100.9', { 'X-Forwarded-For': '203.0.113.7' });

    // The header is attacker-controlled: honouring it with an empty trusted
    // list would let any visitor pick their own visitor hash and rate-limit
    // bucket.
    expect(resolve(ctx)).toBe('198.51.100.9');
    expect(createClientIpResolver([])(ctx)).toBe('198.51.100.9');
  });

  it('takes the first forwarded hop from a trusted peer', () => {
    const resolve = createClientIpResolver(['198.51.100.9']);
    const ctx = context('198.51.100.9', { 'X-Forwarded-For': '203.0.113.7, 198.51.100.9' });

    expect(resolve(ctx)).toBe('203.0.113.7');
  });

  it('trusts a CIDR range and falls back to the peer without a header', () => {
    const resolve = createClientIpResolver(['198.51.100.0/24']);

    expect(resolve(context('198.51.100.42', { 'X-Forwarded-For': '203.0.113.7' }))).toBe('203.0.113.7');
    expect(resolve(context('198.51.100.42'))).toBe('198.51.100.42');
    expect(resolve(context('192.0.2.5', { 'X-Forwarded-For': '203.0.113.7' }))).toBe('192.0.2.5');
  });

  it('answers "unknown" when the socket exposes no peer', () => {
    expect(createClientIpResolver([])(context(undefined))).toBe('unknown');
  });
});
