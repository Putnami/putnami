import { describe, expect, it } from 'bun:test';
import { bounded, MAX_UTM_LEN, utf8Bytes as clientUtf8Bytes } from '../src/client/bounds';
import { utf8Bytes } from '../src/server/sanitize/vocabulary';

const SAMPLES = ['', '/docs', 'я'.repeat(100), 'é', '😀', 'a😀b', '中文', 'ÿ'];

describe('utf8Bytes (browser copy)', () => {
  // The browser cannot use Buffer.byteLength. A copy that disagreed with the
  // server would let the tracker emit values the sanitizer rejects, and no
  // ASCII fixture could reveal it.
  it('agrees with the server measurement on every sample', () => {
    for (const sample of SAMPLES) {
      expect(clientUtf8Bytes(sample)).toBe(utf8Bytes(sample));
    }
  });

  it('counts bytes, not UTF-16 code units', () => {
    const campaign = 'я'.repeat(100);

    expect(campaign.length).toBe(100);
    expect(clientUtf8Bytes(campaign)).toBe(200);
  });
});

describe('bounded', () => {
  it('returns the value untouched when it already fits', () => {
    expect(bounded('summer-sale', MAX_UTM_LEN)).toBe('summer-sale');
  });

  it('truncates to the byte bound without splitting a code point', () => {
    const truncated = bounded('я'.repeat(100), MAX_UTM_LEN);

    expect(clientUtf8Bytes(truncated)).toBe(MAX_UTM_LEN);
    expect(truncated).toBe('я'.repeat(64));
  });

  it('never emits a lone surrogate', () => {
    // A four-byte code point at an odd boundary must be dropped whole; half of
    // one is invalid UTF-8 and Postgres refuses to store it.
    const truncated = bounded('😀😀', 5);

    expect(truncated).toBe('😀');
    expect(clientUtf8Bytes(truncated)).toBe(4);
  });
});
