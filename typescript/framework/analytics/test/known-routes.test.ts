import { describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { loadKnownRoutes, toFileRoute } from '../src/server/http/known-routes';

/**
 * A fragment shaped exactly like the one `@putnami/web`'s pre-build hook
 * writes: expanded catch-alls appear as exact paths, a parameterized page as a
 * `template`, and every non-page mount carries a different `sourceKind`.
 */
const FRAGMENT = {
  routes: [
    {
      match: 'exact',
      path: '/',
      methods: ['GET', 'HEAD'],
      publicEdge: true,
      provenance: {
        project: 'demo',
        package: '@putnami/web',
        sourceKind: 'file-route',
        evidencePath: 'src/app/page.tsx',
      },
    },
    {
      match: 'template',
      path: '/tasks/{id}',
      methods: ['GET', 'HEAD'],
      publicEdge: true,
      provenance: {
        project: 'demo',
        package: '@putnami/web',
        sourceKind: 'file-route',
        evidencePath: 'src/app/tasks/[id]/page.tsx',
      },
    },
    {
      match: 'template',
      path: '/docs/{page...}',
      methods: ['GET', 'HEAD'],
      publicEdge: true,
      provenance: {
        project: 'demo',
        package: '@putnami/web',
        sourceKind: 'file-route',
        evidencePath: 'src/app/docs/[...page]/page.tsx',
      },
    },
    {
      match: 'template',
      path: '/tasks/{id}',
      methods: ['POST'],
      publicEdge: true,
      provenance: {
        project: 'demo',
        package: '@putnami/web',
        sourceKind: 'file-route',
        evidencePath: 'src/app/tasks/[id]/action.ts',
      },
    },
    {
      match: 'prefix',
      path: '/assets/',
      methods: ['GET', 'HEAD'],
      publicEdge: true,
      provenance: {
        project: 'demo',
        package: '@putnami/web',
        sourceKind: 'static-mount',
        evidencePath: '.gen/public/assets',
      },
    },
  ],
};

function withFragment(content?: string): string {
  const root = mkdtempSync(join(tmpdir(), 'putnami-analytics-routes-'));
  if (content !== undefined) {
    mkdirSync(join(root, '.gen', 'http-routes.d'), { recursive: true });
    writeFileSync(join(root, '.gen', 'http-routes.d', 'web.json'), content);
  }
  return root;
}

describe('toFileRoute', () => {
  it('converts both generated template spellings to the file-route form', () => {
    expect(toFileRoute('/')).toBe('/');
    expect(toFileRoute('/tasks/{id}')).toBe('/tasks/[id]');
    expect(toFileRoute('/docs/{page...}')).toBe('/docs/[...page]');
    expect(toFileRoute('/teams/{team}/tasks/{id}')).toBe('/teams/[team]/tasks/[id]');
  });
});

describe('loadKnownRoutes', () => {
  it('keeps file routes only, deduplicated, in file-route form', () => {
    const root = withFragment(JSON.stringify(FRAGMENT));
    try {
      const known = loadKnownRoutes(root);

      expect([...known].sort()).toEqual(['/', '/docs/[...page]', '/tasks/[id]']);
      expect(known.has('/assets/')).toBe(false);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it('yields the empty set when the build wrote no fragment', () => {
    const root = withFragment();
    try {
      expect(loadKnownRoutes(root).size).toBe(0);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it('yields the empty set rather than throwing on an unreadable fragment', () => {
    const root = withFragment('{ this is not json');
    try {
      expect(loadKnownRoutes(root).size).toBe(0);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it('ignores a fragment with no routes member', () => {
    const root = withFragment(JSON.stringify({ generated: true }));
    try {
      expect(loadKnownRoutes(root).size).toBe(0);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
