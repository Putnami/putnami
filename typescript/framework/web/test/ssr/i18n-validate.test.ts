import { afterEach, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { flagValues, localDay, type ValidationSession, validate, wrap } from '../../bin/i18n';
import { type CatalogEntry, type CatalogFile, serializeCatalogFile } from '../../src/ssr/i18n/catalog';

const feature = 'typescript/web-localization';
const requirement = 'human-validation';
const messagesModule = join(import.meta.dir, '../../src/shared/i18n/messages');

let root: string | undefined;

afterEach(() => {
  if (root) rmSync(root, { recursive: true, force: true });
  root = undefined;
});

/**
 * A project with two namespaces: `home` holds a draft title, a stale lede, a
 * draft with a wrong argument and a missing entry; `pricing` holds two drafts.
 */
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
    'src/app/pricing.messages.ts': `import { messages } from '${messagesModule}';
export const pricing = messages('pricing', {
  free: 'Free',
  team: 'Team',
});`,
    'locales/fr/home.json': serializeCatalogFile({
      title: { text: 'Tarifs', source: 'Pricing', state: 'draft' },
      lede: { text: 'Créez un espace en une heure.', source: 'Build a workspace in an hour.', state: 'draft' },
      seats: { text: '{n} sièges', source: '{count} seats', state: 'draft' },
    }),
    'locales/fr/pricing.json': serializeCatalogFile({
      free: { text: 'Gratuit', source: 'Free', state: 'draft' },
      team: { text: 'Équipe', source: 'Team', state: 'draft' },
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
): ValidationSession & { lines: string[]; questions: string[] } {
  const lines: string[] = [];
  const questions: string[] = [];
  return {
    interactive: true,
    env: {},
    identity: () => ({ name: 'Ada Lovelace', email: 'ada@example.com' }),
    today: () => '2026-11-02',
    ask: async (question) => {
      questions.push(question);
      return replies.shift() ?? 'q';
    },
    log: (line) => lines.push(line),
    columns: 80,
    lines,
    questions,
    ...overrides,
  };
}

const readCatalog = (dir: string, namespace = 'home') =>
  JSON.parse(readFileSync(join(dir, `locales/fr/${namespace}.json`), 'utf8')) as CatalogFile;

const validated = (text: string, source: string): CatalogEntry => ({
  text,
  source,
  state: 'validated',
  validatedBy: 'Ada Lovelace <ada@example.com>',
  validatedAt: '2026-11-02',
});

describe('putnami i18n validate', () => {
  specTest(
    'validates the translations of a namespace with one confirmation',
    { feature, requirement, check: 'validate-a-namespace-with-one-confirmation' },
    async () => {
      const dir = project();
      const pricing = readFileSync(join(dir, 'locales/fr/pricing.json'), 'utf8');
      const person = session(['a']);
      expect(await validate(dir, { locale: 'fr', namespaces: ['home'] }, person)).toBe(0);
      expect(person.questions).toEqual(['Validate all 2? [a]ll, [o]ne by one, [s]kip, [q]uit: ']);
      expect(person.lines).toEqual([
        '[1/1] locales/fr/home.json: 2 translation(s) to validate',
        '  key           en                               fr',
        '  ------------  -------------------------------  -------------------------------',
        '  lede (stale)  Build a workspace in minutes.    Créez un espace en une heure.',
        '                (translated from: Build a',
        '                workspace in an hour.)',
        '  title         Pricing                          Tarifs',
        '  skipped seats: uses {n} where the default text uses {count}; correct it in locales/fr/home.json first',
        '  wrote locales/fr/home.json',
        '2 translation(s) validated by Ada Lovelace',
      ]);
      const catalog = readCatalog(dir);
      expect(catalog['title']).toEqual(validated('Tarifs', 'Pricing'));
      // A stale translation, once accepted, is made from the current default text.
      expect(catalog['lede']).toEqual(validated('Créez un espace en une heure.', 'Build a workspace in minutes.'));
      expect(catalog['seats']?.state).toBe('draft');
      expect(catalog['plans']?.state).toBe('missing');
      // The namespace filter leaves every other catalog as it was.
      expect(readFileSync(join(dir, 'locales/fr/pricing.json'), 'utf8')).toBe(pricing);
    },
  );

  specTest(
    'offers each catalog in turn, one by one on request',
    { feature, requirement, check: 'validate-offers-one-by-one-review' },
    async () => {
      const dir = project();
      const person = session(['o', 'n', 'y', 'a']);
      expect(await validate(dir, {}, person)).toBe(0);
      expect(person.questions).toEqual([
        'Validate all 2? [a]ll, [o]ne by one, [s]kip, [q]uit: ',
        'Validate? [y]es, [n]o, [q]uit: ',
        'Validate? [y]es, [n]o, [q]uit: ',
        'Validate all 2? [a]ll, [o]ne by one, [s]kip, [q]uit: ',
      ]);
      expect(person.lines).toContain(
        '[1/2] locales/fr/home.json lede (stale)\n  en, translated from: Build a workspace in an hour.\n  en: Build a workspace in minutes.\n  fr: Créez un espace en une heure.',
      );
      expect(person.lines).toContain('[2/2] locales/fr/pricing.json: 2 translation(s) to validate');
      // A declined stale translation stays stale once its catalog is rewritten.
      expect(readCatalog(dir)['lede']?.state).toBe('stale');
      expect(readCatalog(dir)['title']).toEqual(validated('Tarifs', 'Pricing'));
      expect(readCatalog(dir, 'pricing')['free']).toEqual(validated('Gratuit', 'Free'));
      expect(readCatalog(dir, 'pricing')['team']).toEqual(validated('Équipe', 'Team'));
      expect(person.lines.at(-1)).toBe('3 translation(s) validated by Ada Lovelace');
    },
  );

  specTest(
    'locks an accepted draft with the git identity and the day',
    { feature, requirement, check: 'validate-locks-an-accepted-draft-with-the-git-identity' },
    async () => {
      const dir = project();
      const person = session(['o', 'y', 'y', 's']);
      expect(await validate(dir, { locale: 'fr' }, person)).toBe(0);
      expect(readCatalog(dir)['title']).toEqual(validated('Tarifs', 'Pricing'));
      expect(readCatalog(dir)['lede']).toMatchObject({ state: 'validated', source: 'Build a workspace in minutes.' });
      // A skipped catalog keeps its drafts.
      expect(readCatalog(dir, 'pricing')['free']?.state).toBe('draft');
      expect(person.lines.at(-1)).toBe('2 translation(s) validated by Ada Lovelace');
    },
  );

  it('keeps a declined translation a draft and stops everything at quit', async () => {
    const dir = project();
    const person = session(['o', 'n', 'q']);
    expect(await validate(dir, {}, person)).toBe(0);
    expect(person.questions).toHaveLength(3);
    // Quit at the title: its entry and the stale lede keep the text they were translated from.
    expect(readCatalog(dir)['title']?.state).toBe('draft');
    expect(readCatalog(dir)['lede']).toEqual({
      text: 'Créez un espace en une heure.',
      source: 'Build a workspace in an hour.',
      state: 'draft',
    });
    expect(readCatalog(dir, 'pricing')['free']?.state).toBe('draft');
    expect(person.lines.at(-1)).toBe('0 translation(s) validated by Ada Lovelace');
  });

  it('asks about a single translation and stops at quit before the next catalog', async () => {
    const dir = project();
    writeFileSync(
      join(dir, 'locales/fr/home.json'),
      serializeCatalogFile({ title: { text: 'Tarifs', source: 'Pricing', state: 'draft' } }),
    );
    const person = session(['one', 'q']);
    expect(await validate(dir, {}, person)).toBe(0);
    expect(person.questions).toEqual([
      'Validate this translation? [a]ll, [o]ne by one, [s]kip, [q]uit: ',
      'Validate? [y]es, [n]o, [q]uit: ',
    ]);
    expect(readCatalog(dir)['title']?.state).toBe('draft');
    expect(readCatalog(dir, 'pricing')['free']?.state).toBe('draft');
    expect(person.lines.at(-1)).toBe('0 translation(s) validated by Ada Lovelace');
  });

  it('stops at quit at a catalog prompt and keeps what was validated', async () => {
    const dir = project();
    const person = session(['a', 'q']);
    expect(await validate(dir, {}, person)).toBe(0);
    expect(person.questions).toHaveLength(2);
    expect(readCatalog(dir)['title']?.state).toBe('validated');
    expect(readCatalog(dir, 'pricing')['free']?.state).toBe('draft');
    expect(person.lines.at(-1)).toBe('2 translation(s) validated by Ada Lovelace');
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
        const person = session(['a', 'a'], overrides);
        expect(await validate(dir, { locale: 'fr', namespaces: ['home'] }, person)).toBe(1);
        expect(person.lines).toEqual([message]);
        expect(person.questions).toEqual([]);
      }
      expect(readFileSync(join(dir, 'locales/fr/home.json'), 'utf8')).toBe(before);
    },
  );

  it('asks again after a reply it does not know', async () => {
    const dir = project();
    const person = session(['tout', 'o', 'oui', '', 'y', 'q']);
    expect(await validate(dir, { namespaces: ['home'] }, person)).toBe(0);
    expect(person.lines.filter((line) => line === '  answer a, o, s or q')).toHaveLength(1);
    expect(person.lines.filter((line) => line === '  answer y, n or q')).toHaveLength(2);
    // The lede comes first: the person accepted it, then quit at the title.
    expect(readCatalog(dir)['lede']?.state).toBe('validated');
    expect(readCatalog(dir)['title']?.state).toBe('draft');
  });

  it('records the local calendar day', () => {
    expect(localDay(new Date(2026, 10, 3, 0, 30))).toBe('2026-11-03');
  });

  it('refuses a locale that is not published', async () => {
    const dir = project();
    const person = session([]);
    expect(await validate(dir, { locale: 'de' }, person)).toBe(1);
    expect(person.lines).toEqual(['error de is not a published locale other than the default one']);
  });

  it('refuses a namespace the project does not declare', async () => {
    const dir = project();
    const person = session([]);
    expect(await validate(dir, { namespaces: ['pricing', 'faq', 'blog'] }, person)).toBe(1);
    expect(person.lines).toEqual(['error faq, blog are not namespaces this project declares']);
    expect(person.questions).toEqual([]);
  });
});

describe('catalog table', () => {
  it('wraps a text at spaces and keeps every character', () => {
    expect(wrap('Build a workspace in minutes.', 12)).toEqual(['Build a', 'workspace in', 'minutes.']);
    expect(wrap('abcdefghijklmnopqrstuvwxyz', 10)).toEqual(['abcdefghij', 'klmnopqrst', 'uvwxyz']);
    expect(wrap('', 10)).toEqual(['']);
    expect(wrap('🙂🙂🙂🙂🙂🙂', 5)).toEqual(['🙂🙂🙂🙂🙂', '🙂']);
  });
});

describe('--namespace', () => {
  it('reads every occurrence of a repeated flag from the job arguments', () => {
    const args = ['validate', '--locale', 'fr', '--namespace', 'home', '--namespace=pricing', '--putnamiContext', 'x'];
    expect(flagValues(args, { params: { namespace: 'pricing' } }, 'namespace')).toEqual(['home', 'pricing']);
  });

  it('takes no flag and no empty text as a value', () => {
    const args = ['validate', '--namespace', '--putnamiContext', 'x', '--namespace=', '--namespace'];
    expect(flagValues(args, {}, 'namespace')).toEqual([]);
  });

  it('falls back to the job params', () => {
    expect(flagValues(['validate'], { params: { namespace: ['home', 'pricing'] } }, 'namespace')).toEqual([
      'home',
      'pricing',
    ]);
    expect(flagValues(['validate'], { params: { namespace: 'home' } }, 'namespace')).toEqual(['home']);
    expect(flagValues(['validate'], {}, 'namespace')).toEqual([]);
  });
});
