import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import { getMaterializedDocsPaths } from '../src/lib/docs/static-paths.server';

describe('docs static paths', () => {
  specTest(
    'materializes generated docs before invalidating and enumerating static paths',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'docs-enumeration-is-build-order-independent',
      check: 'generated-doc-sources-precede-enumeration',
    },
    async () => {
      const calls: string[] = [];

      const paths = await getMaterializedDocsPaths({
        projectRoot: '/project',
        workspaceRoot: '/workspace',
        materialize: async (options) => {
          calls.push(`materialize:${options.projectRoot}:${options.workspaceRoot}`);
          return { assets: {}, mounted: [], skipped: [] };
        },
        publishSupport: (options) => {
          calls.push(`publishSupport:${options.projectRoot}:${options.workspaceRoot}`);
          return { assets: {}, genPath: '/project/.gen/public/docs/10-support/index.md' };
        },
        invalidate: () => calls.push('invalidate'),
        enumerate: async () => {
          calls.push('enumerate');
          return ['platform', 'platform/deploy', 'support'];
        },
      });

      expect(paths).toEqual(['platform', 'platform/deploy', 'support']);
      // Both generated sources must land before the tree is rescanned, or a cold
      // build enumerates fewer pages than a warm one and emits a different
      // putnami.http-routes.v1 digest.
      expect(calls).toEqual([
        'materialize:/project:/workspace',
        'publishSupport:/project:/workspace',
        'invalidate',
        'enumerate',
      ]);
    },
  );

  specTest(
    'publishes the generated support page before a cold build enumerates docs',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'cold-build-enumerates-the-page',
      check: 'support-page-is-published-before-enumeration',
    },
    async () => {
      const calls: string[] = [];
      await getMaterializedDocsPaths({
        projectRoot: '/project',
        workspaceRoot: '/workspace',
        materialize: async () => ({ assets: {}, mounted: [], skipped: [] }),
        publishSupport: () => {
          calls.push('publish-support');
          return { assets: {}, genPath: '/project/.gen/public/docs/10-support/index.md' };
        },
        invalidate: () => calls.push('invalidate'),
        enumerate: async () => {
          calls.push('enumerate');
          return ['support'];
        },
      });
      expect(calls).toEqual(['publish-support', 'invalidate', 'enumerate']);
    },
  );

  it('does not enumerate paths when materialization fails', async () => {
    let enumerated = false;

    await expect(
      getMaterializedDocsPaths({
        projectRoot: '/project',
        workspaceRoot: '/workspace',
        materialize: () => Promise.reject(new Error('bundle unavailable')),
        publishSupport: () => ({ assets: {}, genPath: '' }),
        enumerate: async () => {
          enumerated = true;
          return [];
        },
      }),
    ).rejects.toThrow('bundle unavailable');
    expect(enumerated).toBe(false);
  });

  it('does not enumerate paths when the support catalog cannot be published', async () => {
    let enumerated = false;

    await expect(
      getMaterializedDocsPaths({
        projectRoot: '/project',
        workspaceRoot: '/workspace',
        materialize: async () => ({ assets: {}, mounted: [], skipped: [] }),
        publishSupport: () => {
          throw new Error('putnami.support.json: entries must be an array');
        },
        enumerate: async () => {
          enumerated = true;
          return [];
        },
      }),
    ).rejects.toThrow('entries must be an array');
    expect(enumerated).toBe(false);
  });
});
