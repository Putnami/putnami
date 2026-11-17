import { describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import {
  canonicalizeHttpRoutes,
  type HttpRouteInput,
  httpRouteAdmitsPath,
  isHttpPageRoute,
  parseAndValidateHttpRoutes,
  serializeCanonicalHttpRoutes,
} from '../../src/http-routes';

const FIXTURES = join(__dirname, '../../../../../protocols/http-routes/fixtures');
const expectations = JSON.parse(readFileSync(join(FIXTURES, 'expectations.json'), 'utf8')) as {
  invalid: Record<string, string>;
};
const binding = {
  feature: 'http-routes/route-inventory-contract',
  requirement: 'a-page-route-carries-what-it-proves',
  check: 'the-typescript-validator-agrees-on-proofs',
};

const page: HttpRouteInput = {
  match: 'template',
  path: '/w/{workspace}',
  methods: ['HEAD', 'GET'],
  publicEdge: true,
  provenance: {
    project: 'example/web',
    package: '@putnami/web',
    sourceKind: 'file-route',
    evidencePath: 'src/app/w/[workspace]/page.tsx',
  },
};

describe('http-routes proves', () => {
  specTest('reproduces the Go digest of the page-proves fixture', binding, () => {
    const text = readFileSync(join(FIXTURES, 'valid', 'page-proves.json'), 'utf8');
    const parsed = parseAndValidateHttpRoutes(text);
    expect(parsed.diagnostics).toEqual([]);
    if (!parsed.manifest) throw new Error('page-proves fixture did not parse');
    const serialized = serializeCanonicalHttpRoutes(parsed.manifest);
    expect(serialized.manifest?.digest).toBe(parsed.manifest.digest);
    const home = serialized.manifest?.routes.find((route) => route.path === '/');
    expect(home?.proves).toEqual([{ feature: 'acme/home', requirement: 'lists-recent-changes', at: '/' }]);
  });

  specTest('rejects every proves fixture with the Go code', binding, () => {
    const names = Object.keys(expectations.invalid).filter((name) => name.startsWith('proves-'));
    expect(names.length).toBeGreaterThan(0);
    for (const name of names) {
      const result = parseAndValidateHttpRoutes(readFileSync(join(FIXTURES, 'invalid', name), 'utf8'));
      expect(result.manifest, name).toBeUndefined();
      expect(
        result.diagnostics.map((diagnostic) => diagnostic.code),
        name,
      ).toContain(expectations.invalid[name]);
    }
  });

  specTest('sorts proves, covers them in the digest, and omits an absent list', binding, () => {
    const a = { feature: 'acme/workspace', requirement: 'lists-changes', at: '/w/acme' };
    const b = { feature: 'acme/workspace', requirement: 'lists-changes', at: '/w/beta' };
    const c = { feature: 'acme/billing', requirement: 'shows-plan', at: '/w/acme' };
    const input = { ...page, proves: [b, c, a] };
    const sorted = canonicalizeHttpRoutes([input]);
    const ordered = canonicalizeHttpRoutes([{ ...page, proves: [c, a, b] }]);
    const bare = canonicalizeHttpRoutes([page]);
    expect(sorted.manifest?.routes[0].proves).toEqual([c, a, b]);
    expect(sorted.manifest?.digest).toBe(ordered.manifest?.digest);
    expect(bare.manifest?.digest).not.toBe(sorted.manifest?.digest);
    expect(Object.hasOwn(bare.manifest?.routes[0] ?? {}, 'proves')).toBe(false);
    expect(input.proves).toEqual([b, c, a]);
  });

  specTest('tells a page route and the paths it admits', binding, () => {
    expect(isHttpPageRoute(page)).toBe(true);
    expect(isHttpPageRoute({ ...page, methods: ['POST'] })).toBe(false);
    expect(isHttpPageRoute({ ...page, provenance: { ...page.provenance, sourceKind: 'typed-api' } })).toBe(false);
    expect(httpRouteAdmitsPath({ match: 'template', path: '/w/{workspace}' }, '/w/acme')).toBe(true);
    expect(httpRouteAdmitsPath({ match: 'template', path: '/w/{workspace}' }, '/w/acme/code')).toBe(false);
    expect(httpRouteAdmitsPath({ match: 'template', path: '/docs/{slug...}' }, '/docs/a/b')).toBe(true);
    expect(httpRouteAdmitsPath({ match: 'exact', path: '/' }, '/')).toBe(true);
    expect(httpRouteAdmitsPath({ match: 'exact', path: '/' }, '/about')).toBe(false);
  });
});
