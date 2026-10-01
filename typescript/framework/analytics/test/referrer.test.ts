import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { classifyReferrer, extractUtm, NO_HOST, normalizePath, truncateUtf8 } from '../src/server/enrich/referrer';
import {
  MAX_PATH_LEN,
  MAX_REFERRER_LEN,
  MAX_UTM_LEN,
  REFERRER_TYPES,
  utf8Bytes,
} from '../src/server/sanitize/vocabulary';

const FIXTURES_DIR = join(__dirname, '../../../../protocols/analytics/fixtures');

interface ReferrerFixture {
  referrer: string;
  origin: string;
  expected: { referrer: string; referrerType: string };
}

const FIXTURES = JSON.parse(readFileSync(join(FIXTURES_DIR, 'referrers.json'), 'utf8')) as ReferrerFixture[];

describe('classifyReferrer', () => {
  it('reads the whole shared corpus', () => {
    expect(FIXTURES).toHaveLength(12);
  });

  for (const fixture of FIXTURES) {
    it(`classifies ${fixture.referrer.slice(0, 60) || '(none)'}`, () => {
      const info = classifyReferrer(fixture.referrer, new URL(fixture.origin).host);

      // The corpus writes an absent referrer as the empty string; the folded
      // value is null, because the column is nullable.
      expect(info.referrer ?? '').toBe(fixture.expected.referrer);
      expect(info.referrerType).toBe(fixture.expected.referrerType);
      expect(REFERRER_TYPES).toContain(info.referrerType);
    });
  }

  it('drops the query and the fragment before storing', () => {
    // A search referrer carries the visitor's query terms, which never land in
    // a row.
    const info = classifyReferrer('https://www.google.com/search?q=my+private+question#top', 'app.example.com');

    expect(info.referrer).toBe('https://www.google.com/search');
    expect(info.referrer).not.toContain('private');
    expect(info.referrerHost).toBe('google.com');
  });

  it('counts the host without www., and __none__ when there is none', () => {
    expect(classifyReferrer('https://www.facebook.com/putnami', 'app.example.com').referrerHost).toBe('facebook.com');
    expect(classifyReferrer(null, 'app.example.com').referrerHost).toBe(NO_HOST);
    expect(classifyReferrer('   ', 'app.example.com').referrerHost).toBe(NO_HOST);
    expect(classifyReferrer('/dashboard', 'app.example.com').referrerHost).toBe('app.example.com');
    expect(classifyReferrer('/dashboard', '').referrerHost).toBe(NO_HOST);
  });

  it('compares hosts with www. stripped on both sides', () => {
    expect(classifyReferrer('https://www.app.example.com/pricing', 'app.example.com').referrerType).toBe('internal');
    expect(classifyReferrer('https://app.example.com/pricing', 'WWW.APP.EXAMPLE.COM').referrerType).toBe('internal');
  });

  it('matches a trailing-dot entry as a prefix and any other as a suffix', () => {
    expect(classifyReferrer('https://google.fr/search', 'app.example.com').referrerType).toBe('search');
    expect(classifyReferrer('https://www.google.co.uk/', 'app.example.com').referrerType).toBe('search');
    expect(classifyReferrer('https://m.facebook.com/x', 'app.example.com').referrerType).toBe('social');
    expect(classifyReferrer('https://notgoogle.com/', 'app.example.com').referrerType).toBe('other');
    expect(classifyReferrer('https://evil-facebook.com/', 'app.example.com').referrerType).toBe('other');
  });

  it('treats an unparseable or non-http referrer as direct', () => {
    // A browser-supplied string is not an oracle: what does not parse is
    // counted as no referrer rather than stored as one.
    for (const raw of ['not a url', 'javascript:alert(1)', 'ftp://files.example.com/x', '//evil.example.com/x']) {
      const info = classifyReferrer(raw, 'app.example.com');
      expect(info.referrerType).toBe('direct');
      expect(info.referrer).toBeNull();
    }
  });

  it('never stores more than the referrer bound', () => {
    const long = `https://long.example.com/${'a'.repeat(MAX_REFERRER_LEN)}`;

    expect(utf8Bytes(classifyReferrer(long, 'app.example.com').referrer ?? '')).toBe(MAX_REFERRER_LEN);
  });
});

describe('extractUtm', () => {
  it('reads the five campaign keys from a query string or a record', () => {
    const query = new URLSearchParams('utm_source=google&utm_medium=cpc&utm_campaign=launch&other=1');

    expect(extractUtm(query)).toEqual({ source: 'google', medium: 'cpc', campaign: 'launch' });
    expect(extractUtm({ utm_content: 'hero-cta', utm_term: 'putnami' })).toEqual({
      content: 'hero-cta',
      term: 'putnami',
    });
    expect(extractUtm(undefined)).toEqual({});
    expect(extractUtm({})).toEqual({});
  });

  it('trims, drops empties, and bounds each value', () => {
    expect(extractUtm({ utm_source: '  google  ', utm_medium: '   ' })).toEqual({ source: 'google' });
    expect(extractUtm({ utm_campaign: 'c'.repeat(MAX_UTM_LEN + 10) }).campaign).toHaveLength(MAX_UTM_LEN);
  });
});

describe('normalizePath', () => {
  it('strips the query and the fragment and bounds the result', () => {
    expect(normalizePath('/tasks/42?filter=open#top')).toBe('/tasks/42');
    expect(normalizePath('/tasks#top?x=1')).toBe('/tasks');
    expect(normalizePath('/')).toBe('/');
    expect(normalizePath('')).toBe('/');
    expect(normalizePath('?q=1')).toBe('/');
    expect(utf8Bytes(normalizePath(`/${'a'.repeat(MAX_PATH_LEN * 2)}`))).toBe(MAX_PATH_LEN);
  });

  it('roots a bare path segment', () => {
    // `ctx.path()` reports `projects`, not `/projects`, while the wire contract
    // requires the leading slash. Storing both spellings would key two `path`
    // counters for one page — one per source.
    expect(normalizePath('projects')).toBe('/projects');
    expect(normalizePath('tasks/42?filter=open')).toBe('/tasks/42');
  });
});

describe('truncateUtf8', () => {
  it('bounds bytes, not UTF-16 units, and never splits a character', () => {
    // 300 Cyrillic characters are 600 UTF-8 bytes: a fold measuring `.length`
    // would keep all 300 and store a value the Go validator rejects.
    const cyrillic = 'я'.repeat(300);
    expect(cyrillic.length).toBe(300);
    expect(utf8Bytes(cyrillic)).toBe(600);

    const bounded = truncateUtf8(cyrillic, MAX_PATH_LEN);
    expect(utf8Bytes(bounded)).toBe(MAX_PATH_LEN);
    expect(bounded.length).toBe(MAX_PATH_LEN / 2);
    expect(bounded).toBe('я'.repeat(MAX_PATH_LEN / 2));

    // An emoji is four bytes: the fold stops before the bound rather than
    // emitting a lone surrogate.
    const emoji = '🙂'.repeat(10);
    const cut = truncateUtf8(emoji, 10);
    expect(utf8Bytes(cut)).toBe(8);
    expect(cut).toBe('🙂🙂');
    expect(truncateUtf8('short', 512)).toBe('short');
  });
});
