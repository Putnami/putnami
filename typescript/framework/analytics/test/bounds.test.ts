import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { bounded, MAX_UTM_LEN, utf8Bytes as clientUtf8Bytes } from '../src/client/bounds';
import { utf8Bytes } from '../src/server/sanitize/vocabulary';

// Each binding proves a requirement of the analytics protocol's feature
// under a check of this package's own.
const UTF8_BYTES = {
  feature: 'analytics/collection-wire-contract',
  requirement: 'lengths-are-utf-8-bytes',
  check: 'the-typescript-measure-counts-utf-8-bytes',
};

const SAMPLES = ['', '/docs', 'я'.repeat(100), 'é', '😀', 'a😀b', '中文', 'ÿ'];

describe('utf8Bytes (browser copy)', () => {
  // The browser cannot use Buffer.byteLength. A copy that disagreed with the
  // server would let the tracker emit values the sanitizer rejects, and no
  // ASCII fixture could reveal it.
  specTest('agrees with the server measurement on every sample', UTF8_BYTES, () => {
    for (const sample of SAMPLES) {
      expect(clientUtf8Bytes(sample)).toBe(utf8Bytes(sample));
    }
  });

  specTest('counts bytes, not UTF-16 code units', UTF8_BYTES, () => {
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
