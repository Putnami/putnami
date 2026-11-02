import { afterEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { sync } from '../../bin/i18n';
import { formatDiagnostic } from '../../src/ssr/i18n/build';
import {
  type Catalog,
  CatalogFormatError,
  checkCatalog,
  parseCatalogFile,
  runtimeMessages,
  serializeCatalogFile,
  sourceHash,
  syncCatalog,
} from '../../src/ssr/i18n/catalog';

const feature = 'typescript/web-localization';

const declared = new Map<string, Record<string, string>>([
  ['home', { title: 'Pricing', seats: '{count, plural, one {# seat} other {# seats}}' }],
]);

function translated(): Catalog {
  return {
    home: {
      title: { text: 'Tarifs', source: sourceHash('Pricing'), state: 'validated', validatedBy: 'ada' },
      seats: {
        text: '{count, plural, one {# siège} other {# sièges}}',
        source: sourceHash('{count, plural, one {# seat} other {# seats}}'),
        state: 'draft',
      },
    },
  };
}

describe('syncCatalog', () => {
  specTest(
    'adds a missing key and removes an orphan',
    { feature, requirement: 'catalog-sync', check: 'a-new-key-is-added-and-an-orphan-removed' },
    () => {
      const current = translated();
      delete current.home?.seats;
      current['old'] = { gone: { text: 'Parti', source: 'x', state: 'draft' } };
      const { catalog, changes } = syncCatalog(declared, current);
      expect(catalog.home?.seats).toEqual({
        text: '',
        source: sourceHash('{count, plural, one {# seat} other {# seats}}'),
        state: 'missing',
      });
      expect(catalog['old']).toBeUndefined();
      expect(changes).toEqual({ added: ['home.seats'], removed: ['old.gone'], staled: [] });
    },
  );

  specTest(
    'marks a translation stale when its default text changes and keeps its text',
    { feature, requirement: 'catalog-sync', check: 'a-changed-source-marks-the-translation-stale-and-keeps-its-text' },
    () => {
      const changed = new Map([['home', { title: 'Plans and pricing', seats: declared.get('home')?.seats as string }]]);
      const { catalog, changes } = syncCatalog(changed, translated());
      expect(catalog.home?.title).toEqual({
        text: 'Tarifs',
        source: sourceHash('Pricing'),
        state: 'stale',
        validatedBy: 'ada',
      });
      expect(changes.staled).toEqual(['home.title']);
      // A second sync keeps it stale and reports nothing new.
      expect(syncCatalog(changed, catalog).changes.staled).toEqual([]);
    },
  );

  it('fills an absent source hash, which acknowledges the current default text', () => {
    const current = translated();
    const title = current.home?.title;
    if (title) {
      title.source = undefined;
      title.state = 'draft';
    }
    expect(syncCatalog(declared, current).catalog.home?.title?.source).toBe(sourceHash('Pricing'));
  });

  it('writes the same bytes for the same inputs, keys sorted', () => {
    const a = serializeCatalogFile(syncCatalog(declared, translated()).catalog.home ?? {});
    const b = serializeCatalogFile(syncCatalog(declared, { home: JSON.parse(a) }).catalog.home ?? {});
    expect(a).toBe(b);
    expect(Object.keys(JSON.parse(a))).toEqual(['seats', 'title']);
  });
});

describe('checkCatalog', () => {
  const run = (catalog: Catalog, strict = true, defaults = declared) => {
    const { catalog: synced, changes } = syncCatalog(defaults, catalog);
    return checkCatalog(defaults, 'fr', synced, changes, strict);
  };

  specTest(
    'fails a production build on a missing translation',
    { feature, requirement: 'catalog-check', check: 'a-missing-translation-in-a-published-locale-fails' },
    () => {
      const current = translated();
      delete current.home?.seats;
      expect(run(current)).toEqual([
        { level: 'error', code: 'missing', locale: 'fr', key: 'home.seats', message: 'no translation' },
      ]);
      expect(run(current, false)[0]?.level).toBe('warning');
    },
  );

  specTest(
    'fails on an invalid ICU message',
    { feature, requirement: 'catalog-check', check: 'an-invalid-icu-message-fails' },
    () => {
      const current = translated();
      if (current.home?.seats) current.home.seats.text = '{count, plural, one {# siège}';
      const [diagnostic] = run(current);
      expect(diagnostic?.code).toBe('invalid-icu');
      expect(diagnostic?.level).toBe('error');
      const broken = new Map([['home', { title: '{oops' }]]);
      expect(run({}, true, broken)[0]?.message).toContain('default text is not valid');
    },
  );

  specTest(
    'fails when a translation uses other arguments than its default text',
    { feature, requirement: 'catalog-check', check: 'an-argument-mismatch-fails' },
    () => {
      const current = translated();
      if (current.home?.seats) current.home.seats.text = '{n, plural, one {# siège} other {# sièges}}';
      expect(run(current)[0]).toMatchObject({ level: 'error', code: 'argument-mismatch', key: 'home.seats' });
    },
  );

  specTest(
    'warns for a stale translation',
    { feature, requirement: 'catalog-check', check: 'a-stale-translation-warns' },
    () => {
      const changed = new Map([['home', { title: 'Plans', seats: declared.get('home')?.seats as string }]]);
      expect(run(translated(), true, changed)).toEqual([
        {
          level: 'warning',
          code: 'stale',
          locale: 'fr',
          key: 'home.title',
          message: 'the default text changed after this translation was written',
        },
      ]);
    },
  );
});

describe('catalog files', () => {
  it('refuses a shape the sync would not write', () => {
    expect(() => parseCatalogFile('home.json', [])).toThrow(CatalogFormatError);
    expect(() => parseCatalogFile('home.json', { title: { text: 'x', state: 'done' } })).toThrow('state');
    expect(() => parseCatalogFile('home.json', { title: { text: 'x', state: 'draft', note: 1 } })).toThrow(
      "unknown field 'note'",
    );
    expect(parseCatalogFile('home.json', { $schema: 'x' })).toEqual({});
  });

  it('serves every translated entry, stale ones included, and no missing one', () => {
    const catalog = translated();
    if (catalog.home?.seats) catalog.home.seats.state = 'stale';
    catalog['empty'] = { a: { text: '', state: 'missing' } };
    expect(runtimeMessages(catalog)).toEqual({
      home: { title: 'Tarifs', seats: '{count, plural, one {# siège} other {# sièges}}' },
    });
  });
});

describe('putnami i18n sync', () => {
  const messagesModule = join(import.meta.dir, '../../src/shared/i18n/messages');
  let root: string | undefined;

  afterEach(() => {
    if (root) rmSync(root, { recursive: true, force: true });
    root = undefined;
  });

  function project(files: Record<string, string>): string {
    root = mkdtempSync(join(tmpdir(), 'putnami-i18n-'));
    const i18n = { locales: ['en', 'fr'], defaultLocale: 'en', siteUrl: 'https://example.com' };
    files['putnami.json'] = JSON.stringify({ name: 'site', options: { '@putnami/web:generate': { i18n } } });
    for (const [path, content] of Object.entries(files)) {
      mkdirSync(dirname(join(root, path)), { recursive: true });
      writeFileSync(join(root, path), content);
    }
    return root;
  }

  specTest(
    'writes one catalog file per namespace and deletes the file of an undeclared one',
    { feature, requirement: 'catalog-sync', check: 'each-namespace-has-its-own-catalog-file' },
    async () => {
      const dir = project({
        'src/app/home.messages.ts': `import { messages } from '${messagesModule}';
export const home = messages('home', { title: 'Pricing' });`,
        'src/docs/sidebar.messages.ts': `import { messages } from '${messagesModule}';
export const sidebar = messages('docs/sidebar', { intro: 'Introduction' });`,
        'locales/fr/home.json': serializeCatalogFile({
          title: { text: 'Tarifs', source: sourceHash('Pricing'), state: 'draft' },
        }),
        'locales/fr/old/gone.json': serializeCatalogFile({ a: { text: 'Parti', state: 'draft' } }),
      });
      const lines: string[] = [];
      expect(await sync(dir, (line) => lines.push(line))).toBe(0);

      expect(JSON.parse(readFileSync(join(dir, 'locales/fr/docs/sidebar.json'), 'utf8'))).toEqual({
        intro: { text: '', source: sourceHash('Introduction'), state: 'missing' },
      });
      expect(existsSync(join(dir, 'locales/fr/old'))).toBe(false);
      expect(lines).toContain('  wrote locales/fr/docs/sidebar.json');
      expect(lines).not.toContain('  wrote locales/fr/home.json');

      lines.length = 0;
      expect(await sync(dir, (line) => lines.push(line))).toBe(0);
      expect(lines[0]).toBe('locales/fr: up to date (0 added, 0 removed, 0 stale)');
    },
  );

  it('names the namespace file in a diagnostic', () => {
    expect(
      formatDiagnostic({ level: 'error', code: 'missing', locale: 'fr', key: 'docs/sidebar.intro', message: 'x' }),
    ).toBe('locales/fr/docs/sidebar.json intro: x');
  });
});
