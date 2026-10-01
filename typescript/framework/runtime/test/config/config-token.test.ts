import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { Config, Default, Int, resetConfigLoader } from '../../src';
import { ContainerContext } from '../../src/inject/container-context';
import { provide } from '../../src/inject/provider';
import { ConfigService } from '../../src/config/config-service';
import type { ConfigSource } from '../../src/config/config-source';
import {
  assertRegisteredConfigDefinitions,
  configToken,
  getRegisteredConfigDefinitions,
  provideConfig,
} from '../../src/config/config-token';

const DatabaseConfig = Config('database', {
  host: Default(String, 'localhost'),
  port: Default(Int, 5432),
});

const CacheConfig = Config('cache', {
  ttl: Default(Int, 300),
  maxSize: Default(Int, 1000),
});

function createInMemorySource(data: Record<string, unknown>): ConfigSource {
  return { name: 'test', priority: 80, load: () => data };
}

describe('configToken', () => {
  describe('token creation', () => {
    it('creates a NamedToken for a config definition', () => {
      const token = configToken(DatabaseConfig);
      expect(token).toBeDefined();
      expect(typeof token).toBe('object');
    });

    it('token has correct name based on config path', () => {
      const token = configToken(DatabaseConfig);
      // Named tokens have a name property
      expect(token.name).toBe('config:database');
    });
  });

  describe('token interning', () => {
    it('returns the same token object for the same config (reference equality)', () => {
      const token1 = configToken(DatabaseConfig);
      const token2 = configToken(DatabaseConfig);
      expect(token1).toBe(token2); // Reference equality, not just deep equality
    });

    it('returns different tokens for different configs', () => {
      const dbToken = configToken(DatabaseConfig);
      const cacheToken = configToken(CacheConfig);
      expect(dbToken).not.toBe(cacheToken);
    });

    it('token interning is critical for DI Map lookup', () => {
      // The DI container uses Map<Token, Provider> with reference equality.
      // Without interning, two configToken(X) calls would create different objects.
      const map = new Map();
      map.set(configToken(DatabaseConfig), 'provider');
      expect(map.get(configToken(DatabaseConfig))).toBe('provider');
    });
  });

  describe('config registry', () => {
    it('registers configs in the registry', () => {
      // Clear by calling configToken for configs
      configToken(DatabaseConfig);
      configToken(CacheConfig);

      const registered = getRegisteredConfigDefinitions();
      expect(registered.length).toBeGreaterThanOrEqual(2);

      const paths = registered.map((c) => c.path);
      expect(paths).toContain('database');
      expect(paths).toContain('cache');
    });
  });
});

describe('assertRegisteredConfigDefinitions', () => {
  it('accepts manifest paths backed by real runtime definitions', () => {
    const definition = Config('manifest-activation-present', { value: String });
    configToken(definition);

    expect(() => assertRegisteredConfigDefinitions(['manifest-activation-present'])).not.toThrow();
  });

  it('reports every manifest path whose defining module was not activated', () => {
    expect(() => assertRegisteredConfigDefinitions(['manifest-activation-z', 'manifest-activation-a'])).toThrow(
      'capability manifest config definitions were not activated: "manifest-activation-a", "manifest-activation-z"',
    );
  });
});

describe('provideConfig', () => {
  beforeEach(() => {
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  afterEach(() => {
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  it('returns a Registration object', () => {
    const registration = provideConfig(DatabaseConfig);
    expect(registration).toBeDefined();
    expect(registration.__brand).toBe('Registration');
  });

  it('registration resolves through ConfigService in a container', async () => {
    const source = createInMemorySource({
      database: { host: 'prod.db', port: 5433 },
    });

    const ctx = new ContainerContext('test');

    // Register ConfigService
    ctx.register(
      provide(ConfigService, () => new ConfigService([source]), {
        onClose: (svc) => svc.close(),
      }),
    );

    // Register the config provider
    ctx.register(provideConfig(DatabaseConfig));

    await ctx.start();

    // Resolve via the config token
    const config = ctx.get(configToken(DatabaseConfig));
    expect(config.host).toBe('prod.db');
    expect(config.port).toBe(5433);

    await ctx.close();
  });

  it('supports path override via params', async () => {
    const source = createInMemorySource({
      database: {
        replica: { host: 'replica.db', port: 5434 },
      },
    });

    const ctx = new ContainerContext('test');
    ctx.register(
      provide(ConfigService, () => new ConfigService([source]), {
        onClose: (svc) => svc.close(),
      }),
    );

    // Register with path override — uses a different token
    const ReplicaConfig = Config('database.replica', DatabaseConfig.schema);
    ctx.register(provideConfig(ReplicaConfig));

    await ctx.start();

    const config = ctx.get(configToken(ReplicaConfig));
    expect(config.host).toBe('replica.db');
    expect(config.port).toBe(5434);

    await ctx.close();
  });

  it('multiple config providers can coexist', async () => {
    const source = createInMemorySource({
      database: { host: 'db.host', port: 5432 },
      cache: { ttl: 600, maxSize: 5000 },
    });

    const ctx = new ContainerContext('test');
    ctx.register(
      provide(ConfigService, () => new ConfigService([source]), {
        onClose: (svc) => svc.close(),
      }),
    );

    ctx.register(provideConfig(DatabaseConfig));
    ctx.register(provideConfig(CacheConfig));

    await ctx.start();

    const dbConfig = ctx.get(configToken(DatabaseConfig));
    const cacheConf = ctx.get(configToken(CacheConfig));

    expect(dbConfig.host).toBe('db.host');
    expect(cacheConf.ttl).toBe(600);
    expect(cacheConf.maxSize).toBe(5000);

    await ctx.close();
  });
});
