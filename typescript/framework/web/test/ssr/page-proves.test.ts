import { afterAll, describe, expect } from 'bun:test';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  canonicalizeHttpRoutes,
  parseAndValidateHttpRoutes,
  serializeCanonicalHttpRoutes,
} from '@putnami/application/http-routes';
import { specTest } from '@putnami/runtime/spectest';
import { buildWebPageHttpRoutes } from '../../src/ssr/http-routes';
import { readPageProves } from '../../src/ssr/page-proves';

const feature = 'typescript/web-application-delivery';
const requirement = 'page-proves';
const directory = mkdtempSync(join(tmpdir(), 'putnami-page-proves-'));
afterAll(() => rmSync(directory, { recursive: true, force: true }));

const review = {
  feature: 'acme/change-review',
  requirement: 'tabs-in-mission-order',
  at: '/w/acme/changes/pr-24/code',
};

const malformed: [string, RegExp][] = [
  [`export const proves = ${JSON.stringify(review)};`, /must be an array/],
  ['export const proves = [];', /at least one requirement/],
  [`export const proves = [${JSON.stringify({ ...review, feature: 'change-review' })}];`, /not a feature ID/],
  [`export const proves = [${JSON.stringify({ ...review, requirement: 'Tabs' })}];`, /not a requirement ID/],
  [`export const proves = [${JSON.stringify({ ...review, check: 'tabs' })}];`, /unknown key/],
  [`export const proves = [${JSON.stringify({ ...review, at: '/w/{workspace}/code' })}];`, /proves\[0\]\.at/],
  [`export const proves = [${JSON.stringify(review)}, ${JSON.stringify(review)}];`, /repeats an earlier entry/],
];

// Every page module is written before the first import: the module resolver
// caches a directory's entries, so a file written later would not resolve.
const modules = new Map<string, string>([
  ['proving', `export default () => null;\nexport const proves = ${JSON.stringify([review])};\n`],
  ['silent', 'export default () => null;\n'],
  ['broken-proving', `throw new Error('boom');\nexport const proves = ${JSON.stringify([review])};\n`],
  ['broken-silent', "throw new Error('boom');\nexport default 1;\n"],
  ['broken-renamed', "throw new Error('boom');\nconst proves = [];\nexport { proves as covered };\n"],
  ['broken-aliased', "throw new Error('boom');\nconst covered = [];\nexport { covered as proves };\n"],
  ['broken-star', "throw new Error('boom');\nexport * from './silent';\n"],
  ...malformed.map(([source], index): [string, string] => [
    `malformed-${index}`,
    `export default () => null;\n${source}\n`,
  ]),
]);
for (const [name, source] of modules) writeFileSync(join(directory, `${name}.ts`), source);

function pageModule(name: string): string {
  if (!modules.has(name)) throw new Error(`no page module ${name}`);
  return join(directory, `${name}.ts`);
}

describe('page proves', () => {
  specTest(
    'reads a page proves export into the page routes that admit it',
    { feature, requirement, check: 'a-page-proves-reaches-its-page-routes' },
    async () => {
      const file = pageModule('proving');
      const proves = await readPageProves(file, 'src/app/w/[workspace]/changes/[change]/code/page.tsx');
      expect(proves).toEqual([review]);
      expect(await readPageProves(pageModule('silent'), 'src/app/page.tsx')).toBeUndefined();

      const routes = buildWebPageHttpRoutes([
        {
          route: '/w/:workspace/changes/:change/code',
          evidencePath: 'src/app/w/[workspace]/changes/[change]/code/page.tsx',
          hasAction: true,
          proves,
        },
      ]);
      const page = routes.find((route) => route.methods.includes('GET'));
      const action = routes.find((route) => route.methods.includes('POST'));
      expect(page?.proves).toEqual([review]);
      expect(action?.proves).toBeUndefined();

      // The canonical inventory carries it and the strict parser reads it back.
      const canonical = canonicalizeHttpRoutes(
        routes.map((route) => ({ ...route, provenance: { project: 'example/web', ...route.provenance } })),
      );
      expect(canonical.diagnostics).toEqual([]);
      const text = serializeCanonicalHttpRoutes({ routes: canonical.manifest?.routes ?? [] }).text ?? '';
      const parsed = parseAndValidateHttpRoutes(text);
      expect(parsed.diagnostics).toEqual([]);
      expect(parsed.manifest?.routes.find((route) => route.methods.includes('GET'))?.proves).toEqual([review]);
    },
  );

  specTest(
    'gives a localized page proves only on the routes that admit its at',
    { feature, requirement, check: 'a-page-proves-reaches-its-page-routes' },
    () => {
      const french = { feature: 'acme/pricing', requirement: 'shows-plans', at: '/fr/tarifs' };
      const proves = [french];
      const routes = buildWebPageHttpRoutes([
        { route: '/pricing', evidencePath: 'src/app/pricing/page.tsx', hasAction: false, proves },
        { route: '/fr/tarifs', evidencePath: 'src/app/pricing/page.tsx', hasAction: false, proves },
      ]);
      expect(routes.find((route) => route.path === '/pricing')?.proves).toBeUndefined();
      expect(routes.find((route) => route.path === '/fr/tarifs')?.proves).toEqual([french]);
    },
  );

  specTest(
    'fails the build on a malformed proves export',
    { feature, requirement, check: 'a-malformed-proves-fails-the-build' },
    async () => {
      for (const [index, [, message]] of malformed.entries()) {
        const promise = readPageProves(pageModule(`malformed-${index}`), 'src/app/x/page.tsx');
        await expect(promise).rejects.toThrow(message);
        await expect(promise).rejects.toThrow(/src\/app\/x\/page\.tsx: invalid proves export/);
      }
      // A page that declares proves but cannot be imported fails too; one that declares none does not.
      const broken = pageModule('broken-proving');
      await expect(readPageProves(broken, 'src/app/broken/page.tsx')).rejects.toThrow(/failed to import: boom/);
      expect(await readPageProves(pageModule('broken-silent'), 'p')).toBeUndefined();
      expect(await readPageProves(pageModule('broken-renamed'), 'p')).toBeUndefined();
      await expect(readPageProves(pageModule('broken-aliased'), 'p')).rejects.toThrow(/failed to import: boom/);
      expect(await readPageProves(pageModule('broken-star'), 'p')).toBeUndefined();
    },
  );

  specTest(
    'fails the build on an at that no route of the page admits',
    { feature, requirement, check: 'an-at-outside-the-page-fails-the-build' },
    () => {
      expect(() =>
        buildWebPageHttpRoutes([
          {
            route: '/w/:workspace/changes/:change/code',
            evidencePath: 'src/app/w/[workspace]/changes/[change]/code/page.tsx',
            hasAction: false,
            proves: [{ ...review, at: '/w/acme/changes/pr-24/files' }],
          },
        ]),
      ).toThrow(
        "src/app/w/[workspace]/changes/[change]/code/page.tsx: proves at '/w/acme/changes/pr-24/files' (acme/change-review#tabs-in-mission-order) is not a path this page serves",
      );
    },
  );
});
