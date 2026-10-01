import { createRequire } from 'node:module';
import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import { Config, ConfigService, Default, Int, Optional, resetConfigLoader, useConfig } from '../../src';
import { registerConfigLoaderResetHook, registerSourceDiscoverer } from '../../src/config';
import { CLOUD_RUNTIME_SPECIFIER, isMissingOptionalCloudRuntime } from '../../src/config/cloud-runtime-loader';
import { getDefaultSources, type ConfigSource } from '../../src/config/config-source';

const BootstrapConfig = Config('server', {
  port: Default(Int, 3000),
  secret: Optional(String),
});

let registeredData: Record<string, unknown> | undefined;
let registeredLoadCount = 0;
let resetHookCalls = 0;

const registeredSourceDiscoverer = (): ConfigSource | undefined => {
  if (!registeredData) {
    return undefined;
  }
  return {
    name: 'registered-cloud',
    priority: 55,
    load: () => {
      registeredLoadCount += 1;
      return registeredData;
    },
  };
};

const resetHook = () => {
  resetHookCalls += 1;
};

describe('config source registration', () => {
  beforeAll(() => {
    registerSourceDiscoverer(registeredSourceDiscoverer);
    registerSourceDiscoverer(registeredSourceDiscoverer);
    registerConfigLoaderResetHook(resetHook);
    registerConfigLoaderResetHook(resetHook);
  });

  let configServerUrlSnapshot: string | undefined;

  beforeEach(() => {
    registeredData = undefined;
    registeredLoadCount = 0;
    resetHookCalls = 0;
    delete process.env.CONFIG_DATA;
    configServerUrlSnapshot = process.env.CONFIG_SERVER_URL;
    delete process.env.CONFIG_SERVER_URL;
    resetConfigLoader();
    resetHookCalls = 0;
  });

  afterEach(() => {
    registeredData = undefined;
    delete process.env.CONFIG_DATA;
    if (configServerUrlSnapshot === undefined) delete process.env.CONFIG_SERVER_URL;
    else process.env.CONFIG_SERVER_URL = configServerUrlSnapshot;
    resetConfigLoader();
  });

  it('does not add registered sources until their discoverer enables them', () => {
    const names = getDefaultSources().map((source) => source.name);
    expect(names).not.toContain('registered-cloud');
  });

  it('deduplicates registered source discoverers', () => {
    registeredData = { server: { port: 4100 } };

    const names = getDefaultSources().map((source) => source.name);

    expect(names.filter((name) => name === 'registered-cloud')).toHaveLength(1);
  });

  it('uses registered sources in standalone and ConfigService loaders', () => {
    registeredData = { server: { port: 4100, secret: 'resolved-secret' } };

    expect(useConfig(BootstrapConfig)).toEqual({ port: 4100, secret: 'resolved-secret' });

    const service = new ConfigService();
    try {
      expect(service.get(BootstrapConfig)).toEqual({ port: 4100, secret: 'resolved-secret' });
      expect(service.describe(BootstrapConfig).value).toEqual({ port: 4100, secret: 'resolved-secret' });
    } finally {
      service.close();
    }

    expect(registeredLoadCount).toBeGreaterThanOrEqual(2);
  });

  it('boots with CONFIG_SERVER_URL set when a registered discoverer serves it', () => {
    registeredData = { server: { port: 4100, secret: 'resolved-secret' } };
    process.env.CONFIG_SERVER_URL = 'http://127.0.0.1:1/api/configs/resolve';

    const names = getDefaultSources().map((source) => source.name);

    expect(names).toContain('registered-cloud');
    expect(useConfig(BootstrapConfig)).toEqual({ port: 4100, secret: 'resolved-secret' });
  });

  it('keeps CONFIG_DATA above registered remote-style sources', () => {
    registeredData = { server: { port: 4100, secret: 'resolved-secret' } };
    process.env.CONFIG_DATA = JSON.stringify({ server: { port: 4200 } });

    expect(useConfig(BootstrapConfig)).toEqual({ port: 4200, secret: 'resolved-secret' });
  });

  it('runs registered reset hooks once per unique hook', () => {
    resetConfigLoader();

    expect(resetHookCalls).toBe(1);
  });

  it('only treats direct cloud runtime resolution misses as optional', () => {
    const directMissing = new Error(
      "Cannot find module '@putnami/cloud/runtime'\nRequire stack:\n- app.js",
    ) as Error & {
      code?: string;
    };
    directMissing.code = 'MODULE_NOT_FOUND';

    const transitiveMissing = new Error(
      "Cannot find module 'missing-dependency'\nRequire stack:\n- node_modules/@putnami/cloud/runtime.js",
    ) as Error & { code?: string };
    transitiveMissing.code = 'MODULE_NOT_FOUND';

    expect(isMissingOptionalCloudRuntime(directMissing)).toBe(true);
    expect(isMissingOptionalCloudRuntime(transitiveMissing)).toBe(false);
  });

  it("recognizes Bun's real ResolveMessage for the missing optional runtime", () => {
    // Resolve the cloud specifier from a path outside the workspace's node_modules
    // chain so it always fails, regardless of whether `@putnami/cloud` is installed.
    // Bun throws a genuine `ResolveMessage`. Its relationship to `Error` depends
    // on the Bun release running the suite (an `Error` subclass since 1.4, a
    // non-Error value below the floor), so the classifier — and this test —
    // stay shape-based rather than asserting the class.
    const outsideRequire = createRequire('/tmp/__putnami_absent__/probe.js');
    let thrown: unknown;
    try {
      outsideRequire(CLOUD_RUNTIME_SPECIFIER);
      throw new Error(`expected ${CLOUD_RUNTIME_SPECIFIER} to be unresolvable from outside the workspace`);
    } catch (error) {
      thrown = error;
    }

    expect((thrown as { constructor?: { name?: string } })?.constructor?.name).toBe('ResolveMessage');
    expect(isMissingOptionalCloudRuntime(thrown)).toBe(true);
  });

  it('treats non-Error resolution failures by message shape', () => {
    const resolveMessageLike = {
      message: `Cannot find module '${CLOUD_RUNTIME_SPECIFIER}' from '/app/tsconfig.js'`,
      code: 'MODULE_NOT_FOUND',
    };

    expect(resolveMessageLike).not.toBeInstanceOf(Error);
    expect(isMissingOptionalCloudRuntime(resolveMessageLike)).toBe(true);
  });

  it('ignores non-error values and error-like objects for other modules', () => {
    expect(isMissingOptionalCloudRuntime(undefined)).toBe(false);
    expect(isMissingOptionalCloudRuntime('Cannot find module')).toBe(false);
    expect(
      isMissingOptionalCloudRuntime({
        message: "Cannot find module 'some-other-package' from '/app/index.js'",
        code: 'MODULE_NOT_FOUND',
      }),
    ).toBe(false);
  });
});
