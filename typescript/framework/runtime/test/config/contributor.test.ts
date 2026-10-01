import { describe, expect, it } from 'bun:test';
import {
  Config,
  configToken,
  getRegisteredConfigDefinitions,
  isConfigContributor,
  Optional,
  registerContributedConfig,
} from '../../src';
import { specTest } from '../../src/spectest';

// Config fields are Optional so these definitions, which register into the
// process-global config registry, can't fail validation if they leak into a
// later test that starts an app — the convention config-token.test.ts follows.

describe('isConfigContributor', () => {
  it('accepts an object exposing a configDefinitions function', () => {
    expect(isConfigContributor({ name: 'core', configDefinitions: () => [] })).toBe(true);
  });

  it('rejects plain plugins and non-objects', () => {
    expect(isConfigContributor({ name: 'plain' })).toBe(false);
    expect(isConfigContributor({ configDefinitions: 'nope' })).toBe(false);
    expect(isConfigContributor(null)).toBe(false);
    expect(isConfigContributor(undefined)).toBe(false);
  });
});

describe('registerContributedConfig', () => {
  specTest(
    'registers a dependency-owned config into the registry',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'contributed-paths',
      check: 'a-contributed-block-registers-under-its-own-path',
    },
    () => {
      registerContributedConfig(Config('contrib-unit-core', { host: Optional(String) }));
      expect(getRegisteredConfigDefinitions().map((c) => c.path)).toContain('contrib-unit-core');
    },
  );

  specTest(
    'is a no-op when the same definition is re-registered',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'contributed-paths',
      check: 're-registering-the-identical-definition-is-a-no-op',
    },
    () => {
      const def = Config('contrib-unit-idempotent', { host: Optional(String) });
      registerContributedConfig(def);
      expect(() => registerContributedConfig(def)).not.toThrow();
    },
  );

  specTest(
    'throws when a different definition claims an already-registered path',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'contributed-paths',
      check: 'a-different-definition-claiming-the-same-path-fails',
    },
    () => {
      configToken(Config('contrib-unit-conflict', { host: Optional(String) })); // workload's own block
      const dependency = Config('contrib-unit-conflict', { other: Optional(String) });
      expect(() => registerContributedConfig(dependency)).toThrow(/declared by both the workload and a dependency/);
    },
  );
});
