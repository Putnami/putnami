import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { Config, Default, Env, Int, resetConfigLoader, Sensitive, useConfig, useRawConfigSection } from '../../src';
import { ConfigService } from '../../src/config/config-service';
import type { ConfigSource } from '../../src/config/config-source';
import { specTest } from '../../src/spectest';

const TestConfig = Config('test', {
  host: Default(String, 'localhost'),
  port: Default(Int, 3000),
});

const SecretConfig = Config('secret', {
  apiKey: Sensitive(String),
  name: Default(String, 'app'),
});

const EnvConfig = Config('envtest', {
  url: Env('TEST_CONFIG_SVC_URL', Default(String, 'http://localhost')),
});

function createInMemorySource(name: string, priority: number, data: Record<string, unknown>): ConfigSource {
  return { name, priority, load: () => data };
}

describe('ConfigService', () => {
  let service: ConfigService;

  beforeEach(() => {
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
    delete process.env.TEST_CONFIG_SVC_URL;
  });

  afterEach(() => {
    service?.close();
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
    delete process.env.TEST_CONFIG_SVC_URL;
  });

  describe('get()', () => {
    it('loads and validates config from sources', () => {
      const source = createInMemorySource('test', 80, {
        test: { host: 'prod.db', port: 5432 },
      });
      service = new ConfigService([source]);

      const config = service.get(TestConfig);
      expect(config.host).toBe('prod.db');
      expect(config.port).toBe(5432);
    });

    it('uses default values when config path is missing', () => {
      service = new ConfigService([createInMemorySource('empty', 80, {})]);

      const config = service.get(TestConfig);
      expect(config.host).toBe('localhost');
      expect(config.port).toBe(3000);
    });

    it('throws on validation failure for required fields', () => {
      const RequiredConfig = Config('required', { value: String });
      service = new ConfigService([createInMemorySource('empty', 80, {})]);

      expect(() => service.get(RequiredConfig)).toThrow(/Config validation failed/);
    });

    it('supports path override', () => {
      const source = createInMemorySource('test', 80, {
        database: {
          primary: { host: 'primary.db', port: 5432 },
          replica: { host: 'replica.db', port: 5433 },
        },
      });
      service = new ConfigService([source]);

      const primary = service.get(TestConfig, { path: 'database.primary' });
      const replica = service.get(TestConfig, { path: 'database.replica' });

      expect(primary.host).toBe('primary.db');
      expect(replica.host).toBe('replica.db');
    });

    it('YAML values override confInit programmatic defaults', () => {
      const source = createInMemorySource('test', 80, {
        test: { host: 'from-yaml', port: 5432 },
      });
      service = new ConfigService([source]);

      const config = service.get(TestConfig, { confInit: { host: 'from-init' } });
      // YAML takes precedence over confInit
      expect(config.host).toBe('from-yaml');
      expect(config.port).toBe(5432);
    });

    it('confInit fills in values not present in YAML', () => {
      const source = createInMemorySource('test', 80, {
        test: { port: 5432 },
      });
      service = new ConfigService([source]);

      const config = service.get(TestConfig, { confInit: { host: 'from-init' } });
      expect(config.host).toBe('from-init');
      expect(config.port).toBe(5432);
    });

    it('resolves Env() descriptors from process.env', () => {
      process.env.TEST_CONFIG_SVC_URL = 'https://prod.api.com';
      service = new ConfigService([createInMemorySource('empty', 80, {})]);

      const config = service.get(EnvConfig);
      expect(config.url).toBe('https://prod.api.com');
    });
  });

  describe('instance-scoped caching', () => {
    it('caches config instances per service', () => {
      const source = createInMemorySource('test', 80, {
        test: { host: 'cached', port: 8080 },
      });
      service = new ConfigService([source]);

      const config1 = service.get(TestConfig);
      const config2 = service.get(TestConfig);
      expect(config1).toEqual(config2);
    });

    it('different service instances have independent caches', () => {
      const source1 = createInMemorySource('s1', 80, {
        test: { host: 'service1', port: 1111 },
      });
      const source2 = createInMemorySource('s2', 80, {
        test: { host: 'service2', port: 2222 },
      });

      const svc1 = new ConfigService([source1]);
      const svc2 = new ConfigService([source2]);

      const config1 = svc1.get(TestConfig);
      const config2 = svc2.get(TestConfig);

      expect(config1.host).toBe('service1');
      expect(config2.host).toBe('service2');

      svc1.close();
      svc2.close();
    });
  });

  describe('invalidate()', () => {
    it('clears cache and forces reload', () => {
      let callCount = 0;
      const source: ConfigSource = {
        name: 'counting',
        priority: 80,
        load: () => {
          callCount++;
          return { test: { host: `load-${callCount}`, port: 3000 } };
        },
      };
      service = new ConfigService([source]);

      const config1 = service.get(TestConfig);
      expect(config1.host).toBe('load-1');

      service.invalidate();

      const config2 = service.get(TestConfig);
      expect(config2.host).toBe('load-2');
    });
  });

  describe('describe()', () => {
    it('returns config description with origins', () => {
      const source = createInMemorySource('yaml-config', 80, {
        test: { host: 'from-yaml', port: 5432 },
      });
      service = new ConfigService([source]);

      const desc = service.describe(TestConfig);
      expect(desc.path).toBe('test');
      expect(desc.value.host).toBe('from-yaml');
      expect(desc.value.port).toBe(5432);
      expect(desc.origins).toBeArray();
      expect(desc.origins.length).toBeGreaterThan(0);
    });

    it('tracks field origins from sources', () => {
      const source = createInMemorySource('my-source', 80, {
        test: { host: 'from-source' },
      });
      service = new ConfigService([source]);

      const desc = service.describe(TestConfig);
      const hostOrigin = desc.origins.find((o) => o.field === 'host');
      expect(hostOrigin).toBeDefined();
      expect(hostOrigin?.value).toBe('from-source');
      expect(hostOrigin?.source).toBe('my-source');
    });

    it('tracks default values', () => {
      service = new ConfigService([createInMemorySource('empty', 80, {})]);

      const desc = service.describe(TestConfig);
      const portOrigin = desc.origins.find((o) => o.field === 'port');
      expect(portOrigin).toBeDefined();
      expect(portOrigin?.source).toBe('default');
    });

    it('tracks env variable origins', () => {
      process.env.TEST_CONFIG_SVC_URL = 'https://env.com';
      service = new ConfigService([createInMemorySource('empty', 80, {})]);

      const desc = service.describe(EnvConfig);
      const urlOrigin = desc.origins.find((o) => o.field === 'url');
      expect(urlOrigin).toBeDefined();
      expect(urlOrigin?.source).toBe('env:TEST_CONFIG_SVC_URL');
      expect(urlOrigin?.value).toBe('https://env.com');
    });

    it('reports an Env(...) value over confInit, as get() resolves it', () => {
      process.env.TEST_CONFIG_SVC_URL = 'https://env.com';
      service = new ConfigService([]);

      const confInit = { url: 'https://init.com' };
      const desc = service.describe(EnvConfig, { confInit });
      const urlOrigin = desc.origins.find((o) => o.field === 'url');
      expect(urlOrigin?.source).toBe('env:TEST_CONFIG_SVC_URL');
      expect(urlOrigin?.value).toBe('https://env.com');
      expect(service.get(EnvConfig, { confInit }).url).toBe('https://env.com');
    });

    it('tracks confInit origins when YAML does not override', () => {
      const source = createInMemorySource('test', 80, {
        test: { port: 5432 },
      });
      service = new ConfigService([source]);

      const desc = service.describe(TestConfig, { confInit: { host: 'from-init' } });
      const hostOrigin = desc.origins.find((o) => o.field === 'host');
      expect(hostOrigin).toBeDefined();
      expect(hostOrigin?.value).toBe('from-init');
      expect(hostOrigin?.source).toBe('confInit');
    });

    specTest(
      'redacts sensitive field values',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'secret-redaction',
        check: 'a-sensitive-field-is-redacted-in-the-config-description',
      },
      () => {
        const source = createInMemorySource('vault', 80, {
          secret: { apiKey: 'super-secret-key', name: 'my-app' },
        });
        service = new ConfigService([source]);

        const desc = service.describe(SecretConfig);
        const keyOrigin = desc.origins.find((o) => o.field === 'apiKey');
        const nameOrigin = desc.origins.find((o) => o.field === 'name');

        expect(keyOrigin).toBeDefined();
        expect(keyOrigin?.value).toBe('***');
        expect(keyOrigin?.source).toBe('vault');

        expect(nameOrigin).toBeDefined();
        expect(nameOrigin?.value).toBe('my-app');
      },
    );

    it('YAML origin takes precedence over confInit in describe', () => {
      const source = createInMemorySource('yaml-source', 80, {
        test: { host: 'from-yaml', port: 5432 },
      });
      service = new ConfigService([source]);

      const desc = service.describe(TestConfig, { confInit: { host: 'from-init' } });
      const hostOrigin = desc.origins.find((o) => o.field === 'host');
      expect(hostOrigin).toBeDefined();
      // YAML source wins over confInit
      expect(hostOrigin?.value).toBe('from-yaml');
      expect(hostOrigin?.source).toBe('yaml-source');
    });
  });

  describe('useConfig() delegation', () => {
    it('delegates to ConfigService when active', () => {
      const source = createInMemorySource('test', 80, {
        test: { host: 'via-service', port: 9999 },
      });
      service = new ConfigService([source]);

      // useConfig() should now delegate to ConfigService
      const config = useConfig(TestConfig);
      expect(config.host).toBe('via-service');
      expect(config.port).toBe(9999);
    });

    it('falls back to standalone when ConfigService is closed', () => {
      const source = createInMemorySource('test', 80, {
        test: { host: 'via-service', port: 9999 },
      });
      service = new ConfigService([source]);
      service.close();

      // After close, useConfig() falls back to standalone path
      process.env.CONFIG_DATA = JSON.stringify({
        test: { host: 'standalone', port: 7777 },
      });

      const config = useConfig(TestConfig);
      expect(config.host).toBe('standalone');
      expect(config.port).toBe(7777);
    });
  });

  describe('close()', () => {
    it('deactivates the global delegation', () => {
      const source = createInMemorySource('test', 80, {
        test: { host: 'active', port: 1234 },
      });
      service = new ConfigService([source]);

      // Before close: useConfig uses ConfigService
      expect(useConfig(TestConfig).host).toBe('active');

      service.close();

      // After close: useConfig uses standalone path with default values
      const config = useConfig(TestConfig);
      expect(config.host).toBe('localhost'); // default value
    });

    it('restores the previous active service when the latest closes', () => {
      const svc1 = new ConfigService([
        createInMemorySource('svc1', 80, {
          test: { host: 'service-1', port: 1111 },
        }),
      ]);
      const svc2 = new ConfigService([
        createInMemorySource('svc2', 80, {
          test: { host: 'service-2', port: 2222 },
        }),
      ]);

      // Latest registered service is active.
      expect(useConfig(TestConfig).host).toBe('service-2');

      // Closing latest should restore previous owner.
      svc2.close();
      expect(useConfig(TestConfig).host).toBe('service-1');

      svc1.close();
    });

    it('does not clear another service delegation when closed out of order', () => {
      const svc1 = new ConfigService([
        createInMemorySource('svc1', 80, {
          test: { host: 'service-1', port: 1111 },
        }),
      ]);
      const svc2 = new ConfigService([
        createInMemorySource('svc2', 80, {
          test: { host: 'service-2', port: 2222 },
        }),
      ]);

      // Close the non-active service first.
      svc1.close();

      // Active service should remain unchanged.
      expect(useConfig(TestConfig).host).toBe('service-2');

      svc2.close();
    });
  });
});

describe('ConfigService.rawSection', () => {
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

  it('reads a raw section from the service sources and delegates useRawConfigSection', () => {
    service = new ConfigService([
      createInMemorySource('managed', 80, {
        database: { protocolVersion: 1, databases: { auth: { engine: 'postgres' } } },
      }),
    ]);

    expect(service.rawSection('database')?.['protocolVersion']).toBe(1);
    // The free function goes through the active service, honoring its sources
    // over the standalone defaults.
    expect(useRawConfigSection('database')?.['protocolVersion']).toBe(1);
    expect(useRawConfigSection('missing')).toBeUndefined();
  });

  it('falls back to the standalone tree after close()', () => {
    service = new ConfigService([createInMemorySource('managed', 80, { database: { fromService: true } })]);
    expect(useRawConfigSection('database')?.['fromService']).toBe(true);

    service.close();
    service = undefined;
    process.env.CONFIG_DATA = JSON.stringify({ database: { fromEnvData: true } });
    resetConfigLoader();
    expect(useRawConfigSection('database')?.['fromEnvData']).toBe(true);
  });
});
