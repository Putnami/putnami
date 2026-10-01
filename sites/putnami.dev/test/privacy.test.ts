import { describe, expect, it } from 'bun:test';
import { Glob } from 'bun';
import { readFileSync } from 'node:fs';
import { specTest } from '@putnami/spectest';
import { joinPath } from '@putnami/utils';
import { siteEvents } from '../src/analytics';

const FEATURE = 'putnami-dev/cookieless-audience-measurement';
const REQUIREMENT = 'what-the-site-records-is-what-the-notice-says';

const SRC_ROOT = joinPath(import.meta.dir, '..', 'src');
const CONF_ROOT = joinPath(import.meta.dir, '..', 'conf');
const PRIVACY_PAGE = joinPath(SRC_ROOT, 'app', 'privacy', 'page.tsx');

const privacySource = (): string => readFileSync(PRIVACY_PAGE, 'utf8');

/**
 * Every action name the site moves, read from the source rather than from a
 * list a maintainer keeps in their head: `track('name'` and `useTrack()`
 * calls, plus the declarative `data-track` attributes.
 */
function trackedNames(): Set<string> {
  const names = new Set<string>();
  for (const file of new Glob('**/*.{ts,tsx}').scanSync({ cwd: SRC_ROOT })) {
    if (file === 'analytics.ts') {
      continue;
    }
    const source = readFileSync(joinPath(SRC_ROOT, file), 'utf8');
    for (const match of source.matchAll(/\btrack\(\s*(?:useContext<[^>]+>\(\)\s*,\s*)?'([a-z0-9_]+)'/g)) {
      names.add(match[1] as string);
    }
    for (const match of source.matchAll(/data-track='([a-z0-9_]+)'/g)) {
      names.add(match[1] as string);
    }
  }
  return names;
}

describe('the audience-measurement notice', () => {
  specTest(
    'names every action the site declares',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-notice-names-every-declared-action' },
    () => {
      const page = privacySource();

      // The notice is the only place a visitor can read what is recorded, so
      // a new action that never reaches it is collection nobody was told about.
      for (const name of Object.keys(siteEvents)) {
        expect(page).toContain(name);
      }
    },
  );

  specTest(
    'declares every action the site actually tracks',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'every-tracked-action-is-declared' },
    () => {
      const declared = new Set(Object.keys(siteEvents));
      const tracked = trackedNames();

      // An undeclared name throws at the call site server-side and is dropped
      // on arrival from the browser — a click that silently measures nothing.
      expect(tracked.size).toBeGreaterThan(0);
      for (const name of tracked) {
        expect(declared).toContain(name);
      }
    },
  );

  specTest(
    'states the retention the configuration actually applies',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-notice-states-the-configured-retention' },
    () => {
      const page = privacySource();
      // The plugin defaults, which this site does not override. A conf that
      // changed one without the notice following would be a false statement to
      // a visitor, so the two are pinned together here.
      const conf = [...new Glob('.env*.yaml').scanSync({ cwd: CONF_ROOT })]
        .map((file) => readFileSync(joinPath(CONF_ROOT, file), 'utf8'))
        .join('\n');

      expect(conf).not.toContain('retentionRawDays');
      expect(conf).not.toContain('retentionAggregateDays');
      expect(page).toContain('deleted after 90 days');
      expect(page).toContain('kept for 760 days');
    },
  );

  it('points the health probe at the database the site actually provisions', () => {
    const main = readFileSync(joinPath(SRC_ROOT, 'main.ts'), 'utf8');
    const requirements = JSON.parse(
      readFileSync(joinPath(import.meta.dir, '..', 'infra', 'requirements.json'), 'utf8'),
    ) as { databases?: { name: string }[] };
    const declared = (requirements.databases ?? []).map((database) => database.name);

    // `sql()` with no datasource pings `default` on /healthz — a name this
    // workload declares nowhere — so a correctly provisioned instance would
    // still report the database probe unhealthy.
    expect(declared).toEqual(['analytics']);
    expect(main).toContain("sql({ datasource: 'analytics' })");
  });

  it('gives a visitor a way to reach the controller', () => {
    expect(privacySource()).toContain('mailto:contact@putnami.com');
  });

  it('is linked from every page', () => {
    expect(readFileSync(joinPath(SRC_ROOT, 'components', 'footer.tsx'), 'utf8')).toContain("to='/privacy'");
  });
});
