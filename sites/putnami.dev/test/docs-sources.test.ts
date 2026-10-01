import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { specTest } from '@putnami/spectest';
import { fileExists, joinPath } from '@putnami/utils';
import { SUPPORT_SECTION } from '../src/lib/support/publish';

/**
 * Every published documentation section must be traceable to one source that
 * still exists.
 *
 * A `generate.assets` entry whose source was deleted does not fail the build —
 * the TypeScript extension logs `generate asset source not found, skipping` and
 * publishes nothing. That warning is easy to miss in a passing build, and the
 * result is a documentation section that silently disappears while the site
 * keeps declaring its route. This suite turns that into a test failure.
 */

const PROJECT_ROOT = joinPath(import.meta.dir, '..');
const WORKSPACE_ROOT = joinPath(PROJECT_ROOT, '..', '..');

interface AssetEntry {
  from: string;
  to: string;
}

function generateAssets(): AssetEntry[] {
  const config = JSON.parse(readFileSync(joinPath(PROJECT_ROOT, 'putnami.json'), 'utf8')) as {
    options?: { generate?: { assets?: AssetEntry[] } };
  };
  return config.options?.generate?.assets ?? [];
}

describe('published documentation sources', () => {
  it('declares at least the docs, install script, command map, and license copies', () => {
    const targets = generateAssets().map((entry) => entry.to);
    expect(targets).toContain('public/docs');
    expect(targets).toContain('public/install.sh');
    expect(targets).toContain('public/install-commands.txt');
    expect(targets).toContain('public/install.ps1');
    expect(targets).toContain('public/LICENSE.md');
  });

  it('resolves every declared asset source to a path that exists', () => {
    const missing = generateAssets()
      .filter((entry) => !fileExists(joinPath(WORKSPACE_ROOT, entry.from)))
      .map((entry) => `${entry.from} -> ${entry.to}`);

    expect(missing).toEqual([]);
  });

  specTest(
    'publishes every language framework section exactly once from its own language root',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'one-source-per-section',
      check: 'language-sections-use-owning-sources',
    },
    () => {
      // Each language vertical owns its documentation; the site copies it and
      // does not host a second edited copy.
      const assets = generateAssets();
      const expected = [
        { from: '/typescript/doc/framework', to: 'public/docs/09-frameworks/01-typescript' },
        { from: '/go/doc/framework', to: 'public/docs/09-frameworks/02-go' },
        { from: '/python/doc/framework', to: 'public/docs/09-frameworks/03-python' },
      ];
      for (const owner of expected) {
        expect(assets.filter((entry) => entry.to === owner.to)).toEqual([owner]);
      }
    },
  );

  specTest(
    'does not copy any asset into the generated support section',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'one-source-per-section',
      check: 'generated-support-section-has-no-asset-copy',
    },
    () => {
      // The support section has exactly one producer: the support-catalog plugin
      // reading putnami.support.json. An asset copy landing there would give it a
      // second, conflicting source.
      const overlapping = generateAssets().filter((entry) => entry.to.includes(SUPPORT_SECTION));
      expect(overlapping).toEqual([]);
    },
  );

  specTest(
    'keeps generated documentation output out of version control',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'one-source-per-section',
      check: 'generated-doc-output-is-not-versioned',
    },
    () => {
      const tracked = Bun.spawnSync(
        ['git', 'ls-files', '--', 'sites/putnami.dev/.gen', 'sites/putnami.dev/public/docs'],
        { cwd: WORKSPACE_ROOT },
      );
      expect(tracked.exitCode).toBe(0);
      expect(tracked.stdout.toString().trim()).toBe('');
    },
  );
});

describe('public install to serve golden path', () => {
  const commands = [
    'curl -fsSL https://putnami.dev/install.sh | bash',
    'putnami init --project webapp --extension ts',
    'putnami serve webapp',
  ].join('\n');

  it('keeps the root and public getting-started commands identical', () => {
    const rootReadme = readFileSync(joinPath(WORKSPACE_ROOT, 'README.md'), 'utf8');
    const gettingStarted = readFileSync(joinPath(PROJECT_ROOT, 'doc/01-getting-started/index.md'), 'utf8');
    expect(rootReadme).toContain(commands);
    expect(gettingStarted).toContain(commands);
    expect(gettingStarted).toContain('macOS and Linux, on `amd64` or `arm64`');
    expect(rootReadme).toContain('Bun v1.4.0 or later');
    expect(gettingStarted).toContain('Bun v1.4.0');
    expect(gettingStarted).toContain('https://npm.putnami.dev');
    expect(gettingStarted).toContain('.npmrc');
    expect(gettingStarted).toContain('This guarantee is deliberately narrow');
  });

  it('shows the same init and serve argv in the homepage and web-app guide', () => {
    const hero = readFileSync(joinPath(PROJECT_ROOT, 'src/components/hero-terminal.tsx'), 'utf8');
    const guide = readFileSync(joinPath(PROJECT_ROOT, 'doc/03-how-to/01-build-a-web-app.md'), 'utf8');
    for (const command of commands.split('\n').slice(1)) {
      expect(hero).toContain(command);
      expect(guide).toContain(command);
    }
    expect(guide).not.toContain('putnami init --workspace');
    expect(guide).not.toContain('packages/web/');
  });

  it('names the generated plural assistant instructions file', () => {
    const guide = readFileSync(joinPath(PROJECT_ROOT, 'doc/03-how-to/08-develop-with-ai.md'), 'utf8');
    expect(guide).toContain('AGENTS.md');
    expect(guide).not.toMatch(/\bAGENT\.md\b/);
  });

  it('states the --projects and --impacted selection rule on the agents page', () => {
    // The selection rule is what an agent gets wrong most expensively: running
    // --impacted on every iteration rebuilds every dependent, and skipping it
    // before declaring the change complete ships an unverified dependent. The
    // published page has to carry both halves, next to the --impacted paragraph
    // it qualifies, and it has to agree with the generated AGENTS.md guidance.
    // Compared on a single line so a prose re-wrap never fails this.
    const page = readFileSync(joinPath(PROJECT_ROOT, 'doc/06-agents/index.md'), 'utf8').replace(/\s+/g, ' ');
    expect(page).toContain('Run it once, before declaring the change complete.');
    expect(page).toContain('while iterating, select only the projects you changed with `--projects <a>,<b>`');
    expect(page).toContain('The generated `AGENTS.md` gives agents the same rule.');
  });
});

describe('experimental Python boundary', () => {
  const read = (relative: string) => readFileSync(joinPath(PROJECT_ROOT, 'doc', relative), 'utf8');

  specTest(
    'marks Python experimental on the frameworks index',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'python-is-marked-experimental',
      check: 'framework-index-labels-python-experimental',
    },
    () => {
      const page = read('09-frameworks/index.md');
      expect(page).toContain('Python (experimental)');
      expect(page).toContain('no Go/TypeScript parity');
    },
  );

  specTest(
    'states the opt-in, non-default, and no-parity boundary on the frameworks index',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'python-is-marked-experimental',
      check: 'published-python-boundary-is-explicit',
    },
    () => {
      const page = read('09-frameworks/index.md');
      expect(page).toContain('never enabled by default');
      expect(page).toContain('no parity promise');
      expect(page).toMatch(/No Putnami Python\s+framework package family ships today/);
    },
  );

  specTest(
    'requires an explicit opt-in in the getting-started path',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'python-is-marked-experimental',
      check: 'python-getting-started-requires-opt-in',
    },
    () => {
      const page = read('01-getting-started/index.md');
      expect(page).toContain('Experimental Python requires an explicit workspace opt-in');
      expect(page).toContain('putnami deps add @putnami/python');
    },
  );

  specTest(
    'keeps the concepts page from implying Python parity',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'python-is-marked-experimental',
      check: 'concepts-do-not-imply-python-parity',
    },
    () => {
      const page = read('02-concepts/index.md');
      expect(page).toContain('Python does not carry a production, default, or');
      // The framework comparison table covers TypeScript and Go only; adding a
      // Python column would imply a parity the catalog explicitly withholds.
      expect(page).toContain('| Concern | TypeScript | Go |');
    },
  );
});

describe('sparse library release-set guide', () => {
  const guide = readFileSync(joinPath(PROJECT_ROOT, 'doc/03-how-to/15-publish-impacted-libraries.md'), 'utf8');
  const upgrade = readFileSync(joinPath(PROJECT_ROOT, 'doc/03-how-to/10-upgrade-putnami.md'), 'utf8');

  specTest(
    'states that an empty channel needs no bootstrap',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'sparse-release-delivery-is-explicit',
      check: 'an-empty-channel-needs-no-bootstrap',
    },
    () => {
      expect(guide).toContain('There is no bootstrap step');
      expect(guide).toContain('An empty channel resolves to a null head');
      expect(guide).toContain('putnami publish --impacted --channel canary');
      expect(guide).toContain('never falls back from `--impacted` to `--all`');
    },
  );

  it('preserves an unchanged upstream and its exact downstream reference', () => {
    expect(guide).toContain('| `@putnami/core` | `0.1.0-aaa` | No | — |');
    expect(guide).toContain('`@putnami/core@0.1.0-aaa`');
    expect(guide).toContain('neither packaged nor');
    expect(guide).toContain('uploaded');
  });

  specTest(
    'binds deployment and exact upgrades to immutable release-set identity',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'sparse-release-delivery-is-explicit',
      check: 'release-set-handoff-keeps-exact-id-and-digest',
    },
    () => {
      expect(guide).toContain('both `ref.id` and `ref.digest`');
      expect(guide).toContain('must not resolve `canary` again');
      expect(guide).toContain('putnami upgrade --release rs_<64-hex>');
      expect(upgrade).toContain('upgrade --release <id>');
      expect(upgrade).toContain('`--version` remains the compatible selector');
    },
  );

  specTest(
    'keeps the providerless compatibility floor full and non-release-set',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'sparse-release-delivery-is-explicit',
      check: 'cloudless-path-stays-full-and-non-release-set',
    },
    () => {
      expect(guide).toContain('Without a release-set provider, `putnami publish --all` still publishes every');
      expect(guide).toContain('`--channel` is refused');
    },
  );
});
