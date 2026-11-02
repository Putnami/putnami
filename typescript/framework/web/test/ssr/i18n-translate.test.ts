import { afterEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { translate } from '../../bin/i18n';
import { type CatalogFile, serializeCatalogFile } from '../../src/ssr/i18n/catalog';
import { type TranslationWork, WORK_FILE } from '../../src/ssr/i18n/translate';

const feature = 'typescript/web-localization';
const requirement = 'agent-translation';
const messagesModule = join(import.meta.dir, '../../src/shared/i18n/messages');

let root: string | undefined;

afterEach(() => {
  if (root) rmSync(root, { recursive: true, force: true });
  root = undefined;
});

const validated = {
  text: 'Tarifs',
  source: 'Pricing',
  state: 'validated',
  validatedBy: 'Ada <ada@example.com>',
  validatedAt: '2026-11-02',
} as const;

/** A project with one namespace: a validated title, a stale lede, a missing seats count. */
function project(): string {
  root = mkdtempSync(join(tmpdir(), 'putnami-i18n-translate-'));
  const i18n = { locales: ['en', 'fr'], defaultLocale: 'en', siteUrl: 'https://example.com' };
  const files: Record<string, string> = {
    'putnami.json': JSON.stringify({ name: 'site', options: { '@putnami/web:generate': { i18n } } }),
    'src/app/home.messages.ts': `import { messages } from '${messagesModule}';
export const home = messages('home', {
  title: 'Pricing',
  lede: 'Build a Putnami workspace in minutes.',
  seats: '{count, plural, one {# seat} other {# seats}}',
});`,
    'locales/fr/home.json': serializeCatalogFile({
      title: validated,
      lede: { text: 'Créez un espace en une heure.', source: 'Build a workspace in an hour.', state: 'draft' },
    }),
    'locales/fr.glossary.json': JSON.stringify({
      Putnami: { translation: null, note: 'The product name.' },
      workspace: { translation: 'espace de travail' },
      deployment: { translation: 'déploiement' },
    }),
  };
  for (const [path, content] of Object.entries(files)) {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), content);
  }
  return root;
}

const readWork = (dir: string) => JSON.parse(readFileSync(join(dir, WORK_FILE), 'utf8')) as TranslationWork;
const readCatalog = (dir: string) =>
  JSON.parse(readFileSync(join(dir, 'locales/fr/home.json'), 'utf8')) as CatalogFile & { $schema: string };

/** Write answers into the work file, by key. */
function answer(dir: string, texts: Record<string, string | null>): void {
  const work = readWork(dir);
  const home = work.locales['fr']?.['home'];
  if (!home) throw new Error('no home namespace in the work file');
  for (const [key, text] of Object.entries(texts)) {
    const entry = home.entries[key] ?? { source: 'Pricing', text: null };
    home.entries[key] = { ...entry, text };
  }
  writeFileSync(join(dir, WORK_FILE), JSON.stringify(work));
}

describe('putnami i18n translate', () => {
  specTest(
    'lists the missing and stale entries with their context',
    { feature, requirement, check: 'translate-lists-missing-and-stale-entries-with-their-context' },
    async () => {
      const dir = project();
      const lines: string[] = [];
      expect(await translate(dir, false, (line) => lines.push(line))).toBe(0);
      expect(lines[0]).toContain('2 entries to translate');
      expect(readWork(dir)).toEqual({
        protocolVersion: 1,
        locales: {
          fr: {
            home: {
              file: 'src/app/home.messages.ts',
              entries: {
                lede: {
                  source: 'Build a Putnami workspace in minutes.',
                  text: null,
                  previous: { source: 'Build a workspace in an hour.', text: 'Créez un espace en une heure.' },
                },
                seats: { source: '{count, plural, one {# seat} other {# seats}}', text: null },
              },
              context: { title: { source: 'Pricing', text: 'Tarifs' } },
              glossary: {
                Putnami: { translation: null, note: 'The product name.' },
                workspace: { translation: 'espace de travail' },
              },
            },
          },
        },
      });
    },
  );

  it('deletes a leftover work file when nothing is left to translate', async () => {
    const dir = project();
    await translate(dir, false, () => {});
    answer(dir, {
      lede: 'Créez un espace de travail Putnami en quelques minutes.',
      seats: '{count, plural, one {# siège} other {# sièges}}',
    });
    expect(await translate(dir, true, () => {})).toBe(0);
    mkdirSync(dirname(join(dir, WORK_FILE)), { recursive: true });
    writeFileSync(join(dir, WORK_FILE), '{}');
    const lines: string[] = [];
    expect(await translate(dir, false, (line) => lines.push(line))).toBe(0);
    expect(existsSync(join(dir, WORK_FILE))).toBe(false);
    expect(lines).toEqual(['Every published locale is translated; nothing to do']);
  });

  specTest(
    'applies the answers as drafts and refuses a validated entry',
    { feature, requirement, check: 'apply-writes-drafts-and-refuses-a-validated-entry' },
    async () => {
      const dir = project();
      await translate(dir, false, () => {});
      answer(dir, { lede: 'Créez un espace de travail Putnami en quelques minutes.', seats: null });
      const lines: string[] = [];
      expect(await translate(dir, true, (line) => lines.push(line))).toBe(0);
      const catalog = readCatalog(dir);
      expect(catalog.lede).toEqual({
        text: 'Créez un espace de travail Putnami en quelques minutes.',
        source: 'Build a Putnami workspace in minutes.',
        state: 'draft',
      });
      expect(catalog.seats?.state).toBe('missing');
      expect(catalog.title).toEqual(validated);
      expect(existsSync(join(dir, WORK_FILE))).toBe(false);
      expect(lines.at(-1)).toBe('1 draft(s) written; a person validates them with `putnami i18n validate`');

      await translate(dir, false, () => {});
      answer(dir, { title: 'Prix', seats: '{count, plural, one {# siège} other {# sièges}}' });
      const refused: string[] = [];
      expect(await translate(dir, true, (line) => refused.push(line))).toBe(1);
      expect(refused[0]).toBe(
        'error locales/fr/home.json title: a person validated this translation; an agent never rewrites it',
      );
      // One refused answer refuses the file: the valid seats answer is not written either.
      expect(readCatalog(dir).seats?.state).toBe('missing');
      expect(readCatalog(dir).title).toEqual(validated);
    },
  );

  specTest(
    'refuses an answer with other arguments, invalid ICU or an outdated source',
    { feature, requirement, check: 'apply-refuses-an-answer-with-other-arguments' },
    async () => {
      const dir = project();
      await translate(dir, false, () => {});
      answer(dir, { seats: '{n, plural, one {# siège} other {# sièges}}', lede: '{oops' });
      const work = readWork(dir);
      work.locales['fr'] = {
        ...work.locales['fr'],
        gone: { file: '', entries: { a: { source: 'A', text: 'Un' } }, context: {}, glossary: {} },
      };
      writeFileSync(join(dir, WORK_FILE), JSON.stringify(work));
      const lines: string[] = [];
      expect(await translate(dir, true, (line) => lines.push(line))).toBe(1);
      expect(lines.slice(0, 3)).toEqual([
        expect.stringMatching(/^error locales\/fr\/home\.json lede: not valid ICU MessageFormat/),
        'error locales/fr/home.json seats: uses {n} where the default text uses {count}',
        'error locales/fr/gone.json: no messages declaration holds this namespace',
      ]);
      // Nothing is written: the catalog still lacks the seats entry.
      expect(readCatalog(dir).seats).toBeUndefined();

      answer(dir, { seats: '{count, plural, one {# siège} other {# sièges}}', lede: null });
      const outdated = readWork(dir);
      delete outdated.locales['fr']?.['gone'];
      const seats = outdated.locales['fr']?.['home']?.entries['seats'];
      if (seats) seats.source = '{count} seats';
      writeFileSync(join(dir, WORK_FILE), JSON.stringify(outdated));
      const stale: string[] = [];
      expect(await translate(dir, true, (line) => stale.push(line))).toBe(1);
      expect(stale[0]).toBe(
        'error locales/fr/home.json seats: the default text changed after the work file was written; run `putnami i18n translate` again',
      );
    },
  );

  it('refuses to apply without a work file, or one that is not version 1', async () => {
    const dir = project();
    const lines: string[] = [];
    expect(await translate(dir, true, (line) => lines.push(line))).toBe(1);
    expect(lines[0]).toContain('does not exist');
    mkdirSync(dirname(join(dir, WORK_FILE)), { recursive: true });
    writeFileSync(join(dir, WORK_FILE), JSON.stringify({ protocolVersion: 2, locales: {} }));
    lines.length = 0;
    expect(await translate(dir, true, (line) => lines.push(line))).toBe(1);
    expect(lines[0]).toContain('not a version 1 work file');
    writeFileSync(join(dir, WORK_FILE), JSON.stringify({ protocolVersion: 1, locales: { de: {} } }));
    lines.length = 0;
    expect(await translate(dir, true, (line) => lines.push(line))).toBe(1);
    expect(lines[0]).toBe('error de: not a published locale other than the default one');
  });
});
