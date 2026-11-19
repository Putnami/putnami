import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { Config, Default, Int, readConfig, resetConfigLoader, useConfig } from '../../src';
import { ConfigService } from '../../src/config/config-service';
import type { ConfigSource } from '../../src/config/config-source';
import { specTest } from '../../src/spectest';

const LimitsConfig = Config('limits', {
  max: Default(Int, 1),
});

function mutableSource(data: { value: Record<string, unknown> }): ConfigSource {
  return { name: 'mutable', priority: 50, load: () => data.value };
}

describe('readConfig()', () => {
  let service: ConfigService | undefined;

  beforeEach(() => {
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  afterEach(() => {
    service?.close();
    service = undefined;
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  specTest(
    'sees a changed source on the next call',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'fresh-read',
      check: 'read-config-sees-a-changed-source',
    },
    () => {
      process.env.CONFIG_DATA = JSON.stringify({ limits: { max: 2 } });
      expect(readConfig(LimitsConfig).max).toBe(2);
      process.env.CONFIG_DATA = JSON.stringify({ limits: { max: 3 } });
      expect(readConfig(LimitsConfig).max).toBe(3);

      const data = { value: { limits: { max: 4 } } as Record<string, unknown> };
      service = new ConfigService([mutableSource(data)]);
      expect(readConfig(LimitsConfig).max).toBe(4);
      data.value = { limits: { max: 5 } };
      expect(readConfig(LimitsConfig).max).toBe(5);
      expect(service.read(LimitsConfig, { confInit: { max: 9 } }).max).toBe(5);
      data.value = {};
      expect(readConfig(LimitsConfig, { confInit: { max: 9 } }).max).toBe(9);
    },
  );

  specTest(
    'leaves the useConfig cache alone',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'fresh-read',
      check: 'read-config-leaves-the-use-config-cache-alone',
    },
    () => {
      process.env.CONFIG_DATA = JSON.stringify({ limits: { max: 2 } });
      expect(useConfig(LimitsConfig).max).toBe(2);
      process.env.CONFIG_DATA = JSON.stringify({ limits: { max: 3 } });
      expect(readConfig(LimitsConfig).max).toBe(3);
      expect(useConfig(LimitsConfig).max).toBe(2);

      const data = { value: { limits: { max: 4 } } as Record<string, unknown> };
      service = new ConfigService([mutableSource(data)]);
      expect(useConfig(LimitsConfig).max).toBe(4);
      data.value = { limits: { max: 5 } };
      expect(readConfig(LimitsConfig).max).toBe(5);
      expect(useConfig(LimitsConfig).max).toBe(4);
      expect(service.get(LimitsConfig).max).toBe(4);
    },
  );

  specTest(
    'reports a value out of the schema',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'fresh-read',
      check: 'read-config-sees-a-changed-source',
    },
    () => {
      process.env.CONFIG_DATA = JSON.stringify({ limits: { max: 'many' } });
      expect(() => readConfig(LimitsConfig)).toThrow("Config validation failed for 'limits'");
    },
  );

  specTest(
    'falls back to the previous service once the active one closes',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'fresh-read',
      check: 'read-config-sees-a-changed-source',
    },
    () => {
      const outer = new ConfigService([mutableSource({ value: { limits: { max: 6 } } })]);
      const inner = new ConfigService([mutableSource({ value: { limits: { max: 7 } } })]);
      expect(readConfig(LimitsConfig).max).toBe(7);
      inner.close();
      expect(readConfig(LimitsConfig).max).toBe(6);
      outer.close();
      expect(readConfig(LimitsConfig).max).toBe(1);
    },
  );
});
