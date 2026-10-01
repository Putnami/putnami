import { describe, expect, it } from 'bun:test';
import * as rootSurface from '../src/index';
import * as injectSurface from '../src/inject';
import * as robustioSurface from '../src/robustio';
import { specTest } from '../src/spectest';
import * as testingSurface from '../src/testing';

/**
 * Regression test for the public surface of `@putnami/runtime`.
 *
 * The root barrel is a semver contract; test doubles and low-level escape
 * hatches must NOT leak through it. They live behind dedicated subpaths
 * (`/testing`, `/inject`) so consumers opt in explicitly.
 *
 * If you legitimately need a new export at the root, add it here so it
 * remains intentional and reviewable.
 */
describe('@putnami/runtime public surface', () => {
  specTest(
    'does not expose test helpers from the root barrel',
    {
      feature: 'typescript/dependency-injection',
      requirement: 'root-surface',
      check: 'the-root-barrel-hides-test-doubles',
    },
    () => {
      const root = rootSurface as unknown as Record<string, unknown>;
      expect(root.MemoryLogger).toBeUndefined();
      expect(root.MemorySink).toBeUndefined();
      expect(root.buildEntry).toBeUndefined();
      expect(root.syncFetch).toBeUndefined();
      expect(root.setSyncFetchForTest).toBeUndefined();
    },
  );

  specTest(
    'does not expose internal scope/inject escape hatches from the root barrel',
    {
      feature: 'typescript/dependency-injection',
      requirement: 'root-surface',
      check: 'the-root-barrel-hides-container-escape-hatches',
    },
    () => {
      const root = rootSurface as unknown as Record<string, unknown>;
      expect(root.useContainer).toBeUndefined();
      expect(root.resolve).toBeUndefined();
      expect(root.resolveInjection).toBeUndefined();
      expect(root.SCOPE_CONTAINER_KEY).toBeUndefined();
      expect(root.createScopeProxy).toBeUndefined();
    },
  );

  specTest(
    'still exposes the stable inject surface from the root barrel',
    {
      feature: 'typescript/dependency-injection',
      requirement: 'root-surface',
      check: 'the-root-barrel-exposes-the-stable-inject-surface',
    },
    () => {
      const root = rootSurface as unknown as Record<string, unknown>;
      expect(root.Container).toBeDefined();
      expect(root.ScopedContainer).toBeDefined();
      expect(root.ContainerContext).toBeDefined();
      expect(root.createTracingProxy).toBeDefined();
      expect(root.noopTraceSink).toBeDefined();
    },
  );

  it('exposes the config source registration seam from the root barrel', () => {
    const root = rootSurface as unknown as Record<string, unknown>;
    expect(root.registerSourceDiscoverer).toBeFunction();
    expect(root.registerConfigLoaderResetHook).toBeFunction();
  });

  it('keeps cloud-owned runtime symbols off the core package surface', () => {
    const root = rootSurface as unknown as Record<string, unknown>;
    expect(root.RemoteConfigSource).toBeUndefined();
    expect(root.RemoteSecretsSource).toBeUndefined();
    expect(root.EnvVarTokenSource).toBeUndefined();
    expect(root.GcpMetadataTokenSource).toBeUndefined();
    expect(root.discoverTokenSource).toBeUndefined();
  });

  specTest(
    'keeps test helpers reachable via the `testing` subpath',
    {
      feature: 'typescript/dependency-injection',
      requirement: 'root-surface',
      check: 'test-doubles-are-reachable-from-the-testing-subpath',
    },
    () => {
      const testing = testingSurface as unknown as Record<string, unknown>;
      expect(testing.MemoryLogger).toBeDefined();
      expect(testing.MemorySink).toBeDefined();
      // The cross-runtime log conformance harness ships beside MemoryLogger: the
      // boundary tests of application / events / database drive it from here.
      expect(testing.loadCases).toBeFunction();
      expect(testing.findCase).toBeFunction();
      expect(testing.findRecord).toBeFunction();
      expect(testing.assertRecord).toBeFunction();
      expect(testing.compareRecord).toBeFunction();
    },
  );

  it('keeps the Windows-robust file helpers on the robustio subpath only', () => {
    const root = rootSurface as unknown as Record<string, unknown>;
    const robustio = robustioSurface as unknown as Record<string, unknown>;
    for (const name of ['robustRename', 'robustRenameSync', 'robustRemove', 'robustRemoveSync']) {
      expect(robustio[name]).toBeFunction();
      expect(root[name]).toBeUndefined();
    }
  });

  it('does not expose the log conformance harness from the root barrel', () => {
    const root = rootSurface as unknown as Record<string, unknown>;
    expect(root.loadCases).toBeUndefined();
    expect(root.assertRecord).toBeUndefined();
  });

  it('keeps the executable-spec binding off every barrel but its own subpath', () => {
    // `spectest` imports `bun:test` at module load, so it must stay
    // reachable ONLY through `@putnami/runtime/spectest` — re-exporting it
    // from the root or `/testing` would pull the test runner into
    // non-test consumers of those barrels.
    const root = rootSurface as unknown as Record<string, unknown>;
    const testing = testingSurface as unknown as Record<string, unknown>;
    expect(root.specTest).toBeUndefined();
    expect(testing.specTest).toBeUndefined();
  });

  specTest(
    'keeps escape hatches reachable via the `inject` subpath',
    {
      feature: 'typescript/dependency-injection',
      requirement: 'root-surface',
      check: 'escape-hatches-are-reachable-from-the-inject-subpath',
    },
    () => {
      const inject = injectSurface as unknown as Record<string, unknown>;
      expect(inject.useContainer).toBeDefined();
      expect(inject.resolve).toBeDefined();
      expect(inject.resolveInjection).toBeDefined();
      expect(inject.SCOPE_CONTAINER_KEY).toBeDefined();
      expect(inject.createScopeProxy).toBeDefined();
    },
  );
});
