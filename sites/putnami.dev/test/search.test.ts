import { describe, expect, it } from 'bun:test';
import { searchSync } from '../src/lib/search/search';
import { tokenize } from '../src/lib/search/tokenizer';
import type { SearchIndex } from '../src/lib/search/types';
import { FIELD_BODY, FIELD_HEADINGS, FIELD_TITLE } from '../src/lib/search/types';

// ---------------------------------------------------------------------------
// Tokenizer tests
// ---------------------------------------------------------------------------

describe('tokenizer', () => {
  it('should lowercase tokens', () => {
    expect(tokenize('Hello World')).toContain('hello');
    expect(tokenize('Hello World')).toContain('world');
  });

  it('should split on non-alphanumeric characters', () => {
    expect(tokenize('hello, world!')).toContain('hello');
    expect(tokenize('hello, world!')).toContain('world');
  });

  it('should ignore tokens shorter than 2 characters', () => {
    const tokens = tokenize('a b cd ef');
    expect(tokens).not.toContain('a');
    expect(tokens).not.toContain('b');
    expect(tokens).toContain('cd');
    expect(tokens).toContain('ef');
  });

  it('should remove stopwords', () => {
    const tokens = tokenize('the quick and brown fox');
    expect(tokens).not.toContain('the');
    expect(tokens).not.toContain('and');
    expect(tokens).toContain('quick');
    expect(tokens).toContain('brown');
    expect(tokens).toContain('fox');
  });

  it('should split hyphenated words and index joined form', () => {
    const tokens = tokenize('cloud-run');
    expect(tokens).toContain('cloud');
    expect(tokens).toContain('run');
    expect(tokens).toContain('cloudrun');
  });

  it('should split underscore-joined words and index joined form', () => {
    const tokens = tokenize('some_variable');
    expect(tokens).toContain('some');
    expect(tokens).toContain('variable');
    expect(tokens).toContain('somevariable');
  });

  it('should split camelCase identifiers', () => {
    const tokens = tokenize('CloudRun');
    expect(tokens).toContain('cloud');
    expect(tokens).toContain('run');
    expect(tokens).toContain('cloudrun');
  });

  it('should deduplicate tokens', () => {
    const tokens = tokenize('hello hello hello');
    const count = tokens.filter((t) => t === 'hello').length;
    expect(count).toBe(1);
  });

  it('should handle empty input', () => {
    expect(tokenize('')).toEqual([]);
    expect(tokenize('   ')).toEqual([]);
  });

  it('should handle code-ish identifiers', () => {
    const tokens = tokenize('getUserById');
    expect(tokens).toContain('getuserbyid');
    expect(tokens).toContain('get');
    expect(tokens).toContain('user');
  });
});

// ---------------------------------------------------------------------------
// Search ranking tests
// ---------------------------------------------------------------------------

function buildTestIndex(): SearchIndex {
  return {
    version: 'test-v1',
    docs: [
      {
        id: 0,
        url: '/docs/getting-started',
        title: 'Getting Started',
        headings: ['Install', 'Create Project'],
        excerpt: 'Learn how to get started with putnami.',
        breadcrumbs: ['Getting Started'],
      },
      {
        id: 1,
        url: '/docs/cli',
        title: 'CLI Reference',
        headings: ['Commands', 'Options'],
        excerpt: 'The putnami CLI provides workspace management.',
        breadcrumbs: ['CLI'],
      },
      {
        id: 2,
        url: '/docs/routing',
        title: 'Routing',
        headings: ['File-based Routing', 'Dynamic Routes'],
        excerpt: 'Putnami uses file-based routing for pages.',
        breadcrumbs: ['Framework', 'Routing'],
      },
      {
        id: 3,
        url: '/docs/deploy',
        title: 'Deployment',
        headings: ['Docker Deploy', 'Cloud Run'],
        excerpt: 'Deploy your app to production.',
        breadcrumbs: ['How To', 'Deploy'],
      },
    ],
    lex: Object.create(null) as Record<string, [number, number][]>,
  };
}

function addLex(index: SearchIndex, token: string, entries: [number, number][]) {
  index.lex[token] = entries;
}

describe('search ranking', () => {
  it('should rank title matches higher than body matches', () => {
    const index = buildTestIndex();
    // "routing" appears in title for doc 2, and only in body for doc 0
    addLex(index, 'routing', [
      [0, FIELD_BODY],
      [2, FIELD_TITLE],
    ]);

    const results = searchSync(index, 'routing');
    expect(results.length).toBe(2);
    expect(results[0].url).toBe('/docs/routing');
    expect(results[1].url).toBe('/docs/getting-started');
  });

  it('should rank heading matches higher than body matches', () => {
    const index = buildTestIndex();
    addLex(index, 'deploy', [
      [1, FIELD_BODY],
      [3, FIELD_HEADINGS],
    ]);

    const results = searchSync(index, 'deploy');
    expect(results[0].url).toBe('/docs/deploy');
  });

  it('should rank documents matching more query tokens higher', () => {
    const index = buildTestIndex();
    addLex(index, 'cli', [
      [1, FIELD_TITLE],
      [3, FIELD_BODY],
    ]);
    addLex(index, 'commands', [[1, FIELD_HEADINGS]]);

    const results = searchSync(index, 'cli commands');
    expect(results.length).toBe(2);
    // Doc 1 matches both tokens, doc 3 matches only "cli"
    expect(results[0].url).toBe('/docs/cli');
    expect(results[1].url).toBe('/docs/deploy');
  });

  it('should break ties by URL lexicographic order', () => {
    const index = buildTestIndex();
    addLex(index, 'putnami', [
      [0, FIELD_BODY],
      [1, FIELD_BODY],
      [2, FIELD_BODY],
    ]);

    const results = searchSync(index, 'putnami');
    // All have score=1, same token count, so sort by URL
    expect(results[0].url).toBe('/docs/cli');
    expect(results[1].url).toBe('/docs/getting-started');
    expect(results[2].url).toBe('/docs/routing');
  });

  it('should return empty results for no matches', () => {
    const index = buildTestIndex();
    const results = searchSync(index, 'nonexistent');
    expect(results).toEqual([]);
  });

  it('should return empty results for empty query', () => {
    const index = buildTestIndex();
    const results = searchSync(index, '');
    expect(results).toEqual([]);
  });

  it('should respect maxResults limit', () => {
    const index = buildTestIndex();
    addLex(index, 'test', [
      [0, FIELD_BODY],
      [1, FIELD_BODY],
      [2, FIELD_BODY],
      [3, FIELD_BODY],
    ]);

    const results = searchSync(index, 'test', 2);
    expect(results.length).toBe(2);
  });

  it('should accumulate scores across multiple matching fields', () => {
    const index = buildTestIndex();
    // Doc 0: token in title+body (10+1=11), Doc 1: token in headings only (6)
    addLex(index, 'install', [
      [0, FIELD_TITLE | FIELD_BODY],
      [1, FIELD_HEADINGS],
    ]);

    const results = searchSync(index, 'install');
    expect(results[0].url).toBe('/docs/getting-started');
    expect(results[0].score).toBe(11);
    expect(results[1].score).toBe(6);
  });
});

// ---------------------------------------------------------------------------
// Route mapping tests (via build-time indexer)
// ---------------------------------------------------------------------------

describe('route mapping', () => {
  // Test the filePathToUrl logic by importing the indexer and checking the built index
  it('should have correct URL paths in the built index', async () => {
    // Try to load the generated index
    const indexPath = `${import.meta.dir}/../.gen/public/search/index.json`;
    const file = Bun.file(indexPath);
    if (!(await file.exists())) {
      // Index hasn't been built yet, skip
      return;
    }

    const index = (await file.json()) as SearchIndex;
    expect(index.docs.length).toBeGreaterThan(0);

    // All URLs should start with /docs/
    for (const doc of index.docs) {
      expect(doc.url).toMatch(/^\/docs/);
    }

    // URLs should not contain order prefixes (e.g. "01-")
    for (const doc of index.docs) {
      expect(doc.url).not.toMatch(/\/\d+-/);
    }

    // URLs should not contain .md extension
    for (const doc of index.docs) {
      expect(doc.url).not.toContain('.md');
    }
  });

  it('should have non-empty titles for all docs', async () => {
    const indexPath = `${import.meta.dir}/../.gen/public/search/index.json`;
    const file = Bun.file(indexPath);
    if (!(await file.exists())) return;

    const index = (await file.json()) as SearchIndex;
    for (const doc of index.docs) {
      expect(doc.title.length).toBeGreaterThan(0);
    }
  });

  it('should have a valid version hash', async () => {
    const indexPath = `${import.meta.dir}/../.gen/public/search/index.json`;
    const file = Bun.file(indexPath);
    if (!(await file.exists())) return;

    const index = (await file.json()) as SearchIndex;
    expect(index.version).toMatch(/^[a-f0-9]{12}$/);
  });
});

/**
 * Search covered only this project's own `doc/` for a long time, so the
 * framework guides, the tooling section, the platform bundle and the generated
 * support page — about 100 of the 123 published pages — were unreachable from
 * site search while every search test passed. Indexing the published tree
 * instead of one source directory fixes that, and this guard keeps a section
 * from silently dropping back out.
 */
describe('search index coverage', () => {
  it('indexes every published documentation section', async () => {
    const indexPath = `${import.meta.dir}/../.gen/public/search/index.json`;
    const file = Bun.file(indexPath);
    if (!(await file.exists())) return;

    const index = (await file.json()) as SearchIndex;
    const sections = new Set(
      index.docs.map((doc) => doc.url.split('/')[2]).filter((section): section is string => Boolean(section)),
    );

    for (const section of [
      'getting-started',
      'concepts',
      'how-to',
      'principles',
      'frameworks',
      'tooling-&-workspace',
      'platform',
      'support',
    ]) {
      expect(sections).toContain(section);
    }
  });
});
