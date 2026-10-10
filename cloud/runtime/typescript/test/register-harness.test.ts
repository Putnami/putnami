import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import type { ConfigSource } from '@putnami/runtime';
import { cloudSourceDiscoverers, register, type SourceDiscoverer } from '../src/runtime/register';
import { setSyncFetchForTest } from '../src/runtime/sync-fetch';

// Stands in for the framework's getDefaultSources() registry: the loader calls
// register(registrar); registrar.registerSourceDiscoverer collects each
// discoverer; getDefaultSources() then runs them after its five built-in
// file/env sources and appends any non-undefined source. This pins the
// activation contract: priority placement, env-gated discovery, and the
// empty-registry-equivalent path.

const envNames = ['CONFIG_SERVER_URL', 'CONFIG_SERVER_TOKEN', 'APP_NAME', 'APP_ENV', 'K_SERVICE', 'NODE_ENV'] as const;
let envSnapshot: Record<(typeof envNames)[number], string | undefined>;

beforeEach(() => {
  envSnapshot = Object.fromEntries(envNames.map((name) => [name, process.env[name]])) as typeof envSnapshot;
  for (const name of envNames) {
    delete process.env[name];
  }
  // Sources are constructed lazily; nothing fetches here. The stub keeps a
  // stray load() from touching the network if a future assertion adds one.
  setSyncFetchForTest(() => ({ status: 200, body: JSON.stringify({ config: {}, resolved: true, schemaMatch: true }) }));
});

afterEach(() => {
  for (const name of envNames) {
    const value = envSnapshot[name];
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  setSyncFetchForTest(undefined);
});

function collectDiscoverers(): SourceDiscoverer[] {
  const collected: SourceDiscoverer[] = [];
  register({ registerSourceDiscoverer: (discoverer) => collected.push(discoverer) });
  return collected;
}

function discoveredSources(): ConfigSource[] {
  return collectDiscoverers()
    .map((discover) => discover())
    .filter((source): source is ConfigSource => Boolean(source));
}

describe('register() activation contract', () => {
  it('registers exactly the config + secrets discoverers, in priority order', () => {
    expect(collectDiscoverers()).toEqual([...cloudSourceDiscoverers]);
  });

  it('discovers config(50) and secrets(55) when CONFIG_SERVER_URL is a legacy base URL', () => {
    process.env.CONFIG_SERVER_URL = 'https://config.putnami.test';
    process.env.CONFIG_SERVER_TOKEN = 'unit-token';
    process.env.APP_NAME = 'task-api';

    const sources = discoveredSources();
    expect(sources.map((s) => s.name)).toEqual(['config-server', 'secrets-server']);
    expect(sources.map((s) => s.priority)).toEqual([50, 55]);
  });

  it('places config(50) and secrets(55) between the .secrets(35) and CONFIG_DATA(60) built-ins', () => {
    process.env.CONFIG_SERVER_URL = 'https://config.putnami.test';
    process.env.CONFIG_SERVER_TOKEN = 'unit-token';
    process.env.APP_NAME = 'task-api';

    const builtinPriorities = [10, 20, 30, 35, 60]; // the 5 framework file/env sources
    const merged = [...builtinPriorities, ...discoveredSources().map((s) => s.priority)].sort((a, b) => a - b);
    expect(merged).toEqual([10, 20, 30, 35, 50, 55, 60]);
  });

  it('contributes only the config source for an exact /api/configs/resolve URL (secrets merged server-side)', () => {
    process.env.CONFIG_SERVER_URL =
      'https://api.putnami.test/api/configs/resolve?appName=auth%2Fserver&environment=prod&secretsMode=reveal';
    process.env.CONFIG_SERVER_TOKEN = 'unit-token';
    process.env.APP_NAME = 'auth/server';

    expect(discoveredSources().map((s) => s.name)).toEqual(['config-server']);
  });

  it('contributes no sources when CONFIG_SERVER_URL is unset', () => {
    expect(discoveredSources()).toHaveLength(0);
  });

  it('forwards the cache-reset hooks when the registrar accepts them', () => {
    const resetHooks: Array<() => void> = [];
    register({
      registerSourceDiscoverer: () => {},
      registerConfigLoaderResetHook: (reset) => resetHooks.push(reset),
    });
    expect(resetHooks).toHaveLength(3); // config cache, secrets cache, token cache
    for (const reset of resetHooks) {
      reset(); // each hook is callable without throwing
    }
  });
});
