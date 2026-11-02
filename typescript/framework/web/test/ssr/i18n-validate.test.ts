import { afterEach, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { type ValidationSession, validate } from '../../bin/i18n';
import { type CatalogFile, serializeCatalogFile } from '../../src/ssr/i18n/catalog';

const feature = 'typescript/web-localization';
const requirement = 'human-validation';
const messagesModule = join(import.meta.dir, '../../src/shared/i18n/messages');

let root: string | undefined;

afterEach(() => {
  if (root) rmSync(root, { recursive: true, force: true });
  root = undefined;
});

/** A project with a draft title, a stale lede, a draft with a wrong argument and a missing entry. */
function project(): string {
  root = mkdtempSync(join(tmpdir(), 'putnami-i18n-validate-'));
  const i18n = { locales: ['en', 'fr'], defaultLocale: 'en', siteUrl: 'https://example.com' };
  const files: Record<string, string> = {
    'putnami.json': JSON.stringify({ name: 'site', options: { '@putnami/web:generate': { i18n } } }),
    'src/app/home.messages.ts': `import { messages } from '${messagesModule}';
export const home = messages('home', {
  title: 'Pricing',
  lede: 'Build a workspace in minutes.',
  seats: '{count} seats',
  plans: 'Plans',
});`,
    'locales/fr/home.json': serializeCatalogFile({
      title: { text: 'Tarifs', source: 'Pricing', state: 'draft' },
      lede: { text: 'Créez un espace en une heure.', source: 'Build a workspace in an hour.', state: 'draft' },
      seats: { text: '{n} sièges', source: '{count} seats', state: 'draft' },
    }),
  };
  for (const [path, content] of Object.entries(files)) {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), content);
  }
  return root;
}

function session(
  replies: string[],
  overrides: Partial<ValidationSession> = {},
): ValidationSession & { lines: string[] } {
  const lines: string[] = [];
  return {
    interactive: true,
    env: {},
    identity: () => ({ name: 'Ada Lovelace', email: 'ada@example.com' }),
    today: () => '2026-11-02',
    ask: async () => replies.shift() ?? 'q',
    log: (line) => lines.push(line),
    lines,
    ...overrides,
  };
}

const readCatalog = (dir: string) => JSON.parse(readFileSync(join(dir, 'locales/fr/home.json'), 'utf8')) as CatalogFile;

describe('putnami i18n validate', () => {
  specTest(
    'locks an accepted draft with the git identity and the day',
    { feature, requirement, check: 'validate-locks-an-accepted-draft-with-the-git-identity' },
    async () => {
      const dir = project();
      const person = session(['y', 'yes']);
      expect(await validate(dir, 'fr', person)).toBe(0);
      const catalog = readCatalog(dir);
      expect(catalog.title).toEqual({
        text: 'Tarifs',
        source: 'Pricing',
        state: 'validated',
        validatedBy: 'Ada Lovelace <ada@example.com>',
        validatedAt: '2026-11-02',
      });
      // A stale translation, once accepted, is made from the current default text.
      expect(catalog.lede).toMatchObject({ state: 'validated', source: 'Build a workspace in minutes.' });
      expect(catalog.seats?.state).toBe('draft');
      expect(catalog.plans?.state).toBe('missing');
      expect(person.lines).toContain(
        '[1/3] locales/fr/home.json lede (stale)\n  en, translated from: Build a workspace in an hour.\n  en: Build a workspace in minutes.\n  fr: Créez un espace en une heure.',
      );
      expect(person.lines).toContain(
        '  skipped: uses {n} where the default text uses {count}; correct it in locales/fr/home.json first',
      );
      expect(person.lines.at(-1)).toBe('2 translation(s) validated by Ada Lovelace');
    },
  );

  it('keeps a declined translation a draft and stops at quit', async () => {
    const dir = project();
    const person = session(['n', 'q']);
    expect(await validate(dir, undefined, person)).toBe(0);
    expect(readCatalog(dir).title?.state).toBe('draft');
    // Quit before the stale lede: its entry keeps the old default text it was translated from.
    expect(readCatalog(dir).lede).toEqual({
      text: 'Créez un espace en une heure.',
      source: 'Build a workspace in an hour.',
      state: 'draft',
    });
    expect(person.lines.at(-1)).toBe('0 translation(s) validated by Ada Lovelace');
  });

  specTest(
    'refuses without a person at a terminal',
    { feature, requirement, check: 'validate-refuses-without-a-person-at-a-terminal' },
    async () => {
      const dir = project();
      const before = readFileSync(join(dir, 'locales/fr/home.json'), 'utf8');
      const cases: [Partial<ValidationSession>, string][] = [
        [{ interactive: false }, 'error putnami i18n validate needs a person at an interactive terminal'],
        [
          { env: { CLAUDECODE: '1' } },
          'error putnami i18n validate refuses to run inside an agent session (CLAUDECODE is set): a person validates translations',
        ],
        [{ env: { CODEX_THREAD_ID: 't' } }, expect.stringContaining('(CODEX_THREAD_ID is set)') as unknown as string],
        [{ identity: () => undefined }, 'error set git user.name and user.email: validation records who validated'],
      ];
      for (const [overrides, message] of cases) {
        const person = session(['y', 'y'], overrides);
        expect(await validate(dir, 'fr', person)).toBe(1);
        expect(person.lines).toEqual([message]);
      }
      expect(readFileSync(join(dir, 'locales/fr/home.json'), 'utf8')).toBe(before);
    },
  );

  it('refuses a locale that is not published', async () => {
    const dir = project();
    const person = session([]);
    expect(await validate(dir, 'de', person)).toBe(1);
    expect(person.lines).toEqual(['error de is not a published locale other than the default one']);
  });
});
