import { describe, expect, it } from 'bun:test';
import {
  HTTP_ROUTES_ERROR_CODES,
  type HttpRoute,
  type HttpRouteMatch,
  validateHttpRoutes,
} from '../../src/http-routes';

const base = { project: 'example/app', sourceKind: 'manual' } as const;

function route(match: HttpRouteMatch, path: string, publicEdge: boolean): HttpRoute {
  return { match, path, methods: ['GET'], publicEdge, provenance: { ...base } };
}

function overlaps(a: HttpRoute, b: HttpRoute): boolean {
  return validateHttpRoutes([a, b]).some((d) => d.code === HTTP_ROUTES_ERROR_CODES.visibilityOverlap);
}

// Mirrors protocols/http-routes/canonical_test.go TestValidateRoutes_CatchAllOverlapPolicy.
// The over-approximation must never let a public catch-all silently coexist with
// a private route it can match, and must not flag provably-disjoint pairs.
describe('putnami.http-routes.v1 catch-all overlap parity', () => {
  const cases: Array<{ name: string; a: HttpRoute; b: HttpRoute; overlap: boolean }> = [
    {
      name: 'root catch-all covers private exact',
      a: route('template', '/{m...}', true),
      b: route('exact', '/internal/health', false),
      overlap: true,
    },
    {
      name: 'root catch-all covers private prefix',
      a: route('template', '/{m...}', true),
      b: route('prefix', '/internal/', false),
      overlap: true,
    },
    {
      name: 'suffix catch-all disjoint from shorter private',
      a: route('template', '/{m...}/@v/list', true),
      b: route('exact', '/internal/health', false),
      overlap: false,
    },
    {
      name: 'suffix catch-all disjoint by fixed suffix',
      a: route('template', '/{m...}/@v/list', true),
      b: route('exact', '/x/@latest/list', false),
      overlap: false,
    },
    {
      name: 'suffix catch-all matches consistent private exact',
      a: route('template', '/{m...}/@v/list', true),
      b: route('exact', '/x/@v/list', false),
      overlap: true,
    },
    {
      name: 'suffix catch-all matches private template suffix',
      a: route('template', '/{m...}/@v/list', true),
      b: route('template', '/x/@v/{file}', false),
      overlap: true,
    },
    {
      name: 'two catch-alls disjoint by suffix',
      a: route('template', '/{m...}/@v/list', true),
      b: route('template', '/{n...}/@latest', false),
      overlap: false,
    },
    {
      name: 'two catch-alls overlap on shared suffix',
      a: route('template', '/{m...}/@v/list', true),
      b: route('template', '/{n...}/list', false),
      overlap: true,
    },
    {
      name: 'two catch-alls disjoint by prefix',
      a: route('template', '/a/{m...}', true),
      b: route('template', '/b/{n...}', false),
      overlap: false,
    },
    {
      name: 'catch-all boundary length too short is disjoint',
      a: route('template', '/{m...}/@v/list', true),
      b: route('exact', '/a/b', false),
      overlap: false,
    },
  ];

  for (const testCase of cases) {
    it(testCase.name, () => {
      expect(overlaps(testCase.a, testCase.b)).toBe(testCase.overlap);
    });
  }

  it('does not flag a public catch-all over a method-disjoint private route', () => {
    const a: HttpRoute = { ...route('template', '/{m...}', true), methods: ['GET'] };
    const b: HttpRoute = { ...route('exact', '/internal/health', false), methods: ['POST'] };
    expect(validateHttpRoutes([a, b])).toEqual([]);
  });
});
