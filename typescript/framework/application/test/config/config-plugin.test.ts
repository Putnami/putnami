import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { restoreEnv } from '@putnami/utils';
import { ConfigPlugin, config } from '../../src/config/config-plugin';
import type { Module } from '../../src/application';

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;
const ORIGINAL_K_SERVICE = process.env.K_SERVICE;
const ORIGINAL_NODE_ENV = process.env.NODE_ENV;

beforeEach(() => {
  delete process.env.CONFIG_DATA;
  delete process.env.K_SERVICE;
  resetConfigLoader();
});

afterEach(() => {
  restoreEnv('CONFIG_DATA', ORIGINAL_CONFIG_DATA);
  restoreEnv('K_SERVICE', ORIGINAL_K_SERVICE);
  restoreEnv('NODE_ENV', ORIGINAL_NODE_ENV);
  resetConfigLoader();
});

describe('config factory', () => {
  it('creates ConfigPlugin with default env', () => {
    process.env.NODE_ENV = 'production';
    const plugin = config();
    expect(plugin).toBeInstanceOf(ConfigPlugin);
  });

  it('uses test env when NODE_ENV is test', () => {
    process.env.NODE_ENV = 'test';
    const plugin = config();
    expect(plugin).toBeInstanceOf(ConfigPlugin);
  });

  it('uses custom env when provided', () => {
    const plugin = config({ env: 'staging' });
    expect(plugin).toBeInstanceOf(ConfigPlugin);
  });
});

describe('ConfigPlugin.generate', () => {
  it('skips when CONFIG_DATA is set', async () => {
    process.env.CONFIG_DATA = JSON.stringify({ some: 'config' });
    const plugin = config({ env: 'test' });
    const result = await plugin.generate({} as Module);
    expect(result).toEqual({});
  });

  it('skips when K_SERVICE is set', async () => {
    process.env.K_SERVICE = 'my-service';
    const plugin = config({ env: 'test' });
    const result = await plugin.generate({} as Module);
    expect(result).toEqual({});
  });
});
