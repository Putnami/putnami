import { describe, expect, it } from 'bun:test';
import {
  canonicalizeHttpRoutes,
  completeHttpRoutesEvaluation,
  unavailableHttpRoutesEvaluation,
} from '../../src/http-routes';
import { normalizeGeneratedHttpRoutePath } from '../../src/http-routes/generation';

describe('HTTP route evaluation helpers', () => {
  it('serializes a complete evaluation as the exact version 1 literal', () => {
    expect(JSON.stringify(completeHttpRoutesEvaluation())).toBe('{"v":1,"state":"complete"}');
  });

  it('serializes an unavailable evaluation as the exact version 1 literal with its reason', () => {
    expect(JSON.stringify(unavailableHttpRoutesEvaluation('the application factory failed'))).toBe(
      '{"v":1,"state":"unavailable","reason":"the application factory failed"}',
    );
  });

  it('refuses an unavailable evaluation without a nonempty reason', () => {
    for (const reason of ['', '   ', '\n\t']) {
      expect(() => unavailableHttpRoutesEvaluation(reason)).toThrow('needs a nonempty reason');
    }
    expect(() => unavailableHttpRoutesEvaluation(undefined as unknown as string)).toThrow('needs a nonempty reason');
  });
});

describe('normalizeGeneratedHttpRoutePath catch-all wiring', () => {
  it('maps named catch-all spellings to the v1 {name...} template', () => {
    expect(normalizeGeneratedHttpRoutePath('/{module...}/@v/list')).toEqual({
      match: 'template',
      path: '/{module...}/@v/list',
    });
    expect(normalizeGeneratedHttpRoutePath('/files/[...path]')).toEqual({
      match: 'template',
      path: '/files/{path...}',
    });
  });

  it('leaves anonymous wildcards unrepresentable so validation fails closed', () => {
    const normalized = normalizeGeneratedHttpRoutePath('/files/*');
    expect(normalized).toEqual({ match: 'exact', path: '/files/*' });
    const result = canonicalizeHttpRoutes([
      {
        ...normalized,
        methods: ['GET'],
        publicEdge: true,
        provenance: { project: 'example/app', sourceKind: 'file-route' },
      },
    ]);
    expect(result.manifest).toBeUndefined();
    expect(result.diagnostics.map((d) => d.code)).toContain('http_routes.unsupported_pattern');
  });

  it('produces a valid inventory for a gomod-shaped catch-all server', () => {
    const paths: [string, string[]][] = [
      ['/{module...}', ['GET', 'HEAD']],
      ['/{module...}/@v/list', ['GET', 'HEAD']],
      ['/{module...}/@v/{versionfile}', ['GET', 'HEAD']],
      ['/{module...}/@latest', ['GET', 'HEAD']],
      ['/{module...}/@v/{version}', ['PUT']],
      ['/{module...}/-/blobs/upload', ['POST']],
    ];
    const result = canonicalizeHttpRoutes(
      paths.map(([path, methods]) => ({
        ...normalizeGeneratedHttpRoutePath(path),
        methods,
        publicEdge: true,
        provenance: { project: 'example/gomod-server', sourceKind: 'manual' },
      })),
    );
    expect(result.diagnostics).toEqual([]);
    expect(result.manifest?.routes).toHaveLength(6);
  });
});
