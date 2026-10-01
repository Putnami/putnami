import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import {
  Config,
  Default,
  Env,
  Int,
  Optional,
  resetConfigLoader,
  Sensitive,
  useConfig,
  useRawConfigSection,
} from '../../src';
import { redactErrors } from '../../src/config/config';
import { specTest } from '../../src/spectest';

describe('Config and useConfig', () => {
  beforeEach(() => {
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  afterEach(() => {
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
    // Clean up env vars used in tests
    delete process.env.TEST_PORT;
    delete process.env.TEST_SECRET;
  });

  describe('Config()', () => {
    it('creates a config definition with path and schema', () => {
      const TestConfig = Config('test.config', {
        value: Default(String, 'default'),
      });

      expect(TestConfig.path).toBe('test.config');
      expect(TestConfig.schema).toBeDefined();
      expect(TestConfig.schema.value).toBeDefined();
    });
  });

  describe('useConfig', () => {
    it('loads config from CONFIG_DATA environment variable', () => {
      const DatabaseConfig = Config('database', {
        host: Default(String, 'localhost'),
        port: Default(Int, 5432),
      });

      process.env.CONFIG_DATA = JSON.stringify({
        database: {
          host: 'prod.example.com',
          port: 3306,
        },
      });

      const config = useConfig(DatabaseConfig);
      expect(config.host).toBe('prod.example.com');
      expect(config.port).toBe(3306);
    });

    it('uses default values when config is not found', () => {
      const MissingConfig = Config('missing', {
        value: Default(String, 'default'),
      });

      process.env.CONFIG_DATA = JSON.stringify({});

      const config = useConfig(MissingConfig);
      expect(config.value).toBe('default');
    });

    specTest(
      'throws error for required fields that are missing',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'fail-loud',
        check: 'a-missing-required-field-fails-the-read',
      },
      () => {
        const RequiredConfig = Config('required.config', {
          value: String,
        });

        process.env.CONFIG_DATA = JSON.stringify({});

        expect(() => useConfig(RequiredConfig)).toThrow(/Config validation failed/);
      },
    );

    it('caches config instances', () => {
      const CacheTestConfig = Config('cache.test', {
        value: Default(String, 'default'),
      });

      process.env.CONFIG_DATA = JSON.stringify({
        cache: { test: { value: 'cached' } },
      });

      const config1 = useConfig(CacheTestConfig);
      const config2 = useConfig(CacheTestConfig);

      expect(config1).toEqual(config2);
    });

    it('allows path override via params', () => {
      const DatabaseConfig = Config('database', {
        host: Default(String, 'localhost'),
      });

      process.env.CONFIG_DATA = JSON.stringify({
        database: {
          primary: { host: 'primary.db' },
          replica: { host: 'replica.db' },
        },
      });

      const primaryConfig = useConfig(DatabaseConfig, {
        path: 'database.primary',
      });
      const replicaConfig = useConfig(DatabaseConfig, {
        path: 'database.replica',
      });

      expect(primaryConfig.host).toBe('primary.db');
      expect(replicaConfig.host).toBe('replica.db');
    });

    it('loads nested config paths', () => {
      const AuthConfig = Config('services.api.auth', {
        secret: Default(String, 'default-secret'),
      });

      process.env.CONFIG_DATA = JSON.stringify({
        services: {
          api: {
            auth: {
              secret: 'super-secret-key',
            },
          },
        },
      });

      const config = useConfig(AuthConfig);
      expect(config.secret).toBe('super-secret-key');
    });

    it('YAML values override confInit programmatic defaults', () => {
      const AppConfig = Config('app', {
        name: Default(String, 'default'),
        port: Default(Int, 3000),
      });

      process.env.CONFIG_DATA = JSON.stringify({
        app: { name: 'from-yaml', port: 8080 },
      });

      const config = useConfig(AppConfig, {
        confInit: { name: 'from-init' },
      });

      // YAML takes precedence over confInit
      expect(config.name).toBe('from-yaml');
      expect(config.port).toBe(8080);
    });

    it('confInit fills in values not present in YAML', () => {
      const AppConfig = Config('app', {
        name: Default(String, 'default'),
        port: Default(Int, 3000),
      });

      process.env.CONFIG_DATA = JSON.stringify({
        app: { port: 8080 },
      });

      const config = useConfig(AppConfig, {
        confInit: { name: 'from-init' },
      });

      // confInit provides name since YAML doesn't set it
      expect(config.name).toBe('from-init');
      expect(config.port).toBe(8080);
    });

    specTest(
      'validates types and rejects invalid values',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'fail-loud',
        check: 'a-type-mismatch-fails-the-read',
      },
      () => {
        const StrictConfig = Config('strict', {
          port: Int,
        });

        process.env.CONFIG_DATA = JSON.stringify({
          strict: { port: 'not-a-number' },
        });

        expect(() => useConfig(StrictConfig)).toThrow(/Config validation failed/);
      },
    );

    it('supports optional fields', () => {
      const OptConfig = Config('opt', {
        name: Default(String, 'required'),
        extra: Optional(String),
      });

      process.env.CONFIG_DATA = JSON.stringify({
        opt: { name: 'hello' },
      });

      const config = useConfig(OptConfig);
      expect(config.name).toBe('hello');
      expect(config.extra).toBeUndefined();
    });

    specTest(
      'does not pollute Object.prototype from constructor or prototype keys',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'untrusted-input',
        check: 'constructor-and-prototype-keys-cannot-pollute-object-prototype',
      },
      () => {
        const AppConfig = Config('app', { name: Default(String, 'default') });
        // The classic `constructor.prototype` walk, plus a bare `prototype`
        // key, both as *own* keys of the parsed payload. A merge that copies
        // keys blindly reaches Object.prototype through either one.
        process.env.CONFIG_DATA = JSON.stringify({
          app: { name: 'safe' },
          constructor: { prototype: { polluted: 'via-constructor' } },
          prototype: { polluted: 'via-prototype' },
        });

        const config = useConfig(AppConfig);

        expect(config.name).toBe('safe');
        expect((Object.prototype as Record<string, unknown>).polluted).toBeUndefined();
        expect(({} as Record<string, unknown>).polluted).toBeUndefined();
      },
    );

    specTest(
      'does not pollute Object.prototype from a __proto__ config payload',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'untrusted-input',
        check: 'a-proto-key-cannot-pollute-object-prototype',
      },
      () => {
        const AppConfig = Config('app', { name: Default(String, 'default') });
        // Raw JSON string so the parsed object carries an *own* __proto__ key.
        process.env.CONFIG_DATA = '{"app":{"name":"safe"},"__proto__":{"polluted":"yes"}}';

        const config = useConfig(AppConfig);

        expect(config.name).toBe('safe');
        expect(({} as Record<string, unknown>).polluted).toBeUndefined();
        expect((Object.prototype as Record<string, unknown>).polluted).toBeUndefined();
      },
    );
  });

  describe('Env()', () => {
    specTest(
      'resolves values from environment variables',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'env-fills-gaps',
        check: 'an-env-descriptor-supplies-a-missing-value',
      },
      () => {
        const EnvConfig = Config('env.test', {
          port: Env('TEST_PORT', Int),
        });

        process.env.TEST_PORT = '9090';
        process.env.CONFIG_DATA = JSON.stringify({});

        const config = useConfig(EnvConfig);
        expect(config.port).toBe(9090);
      },
    );

    specTest(
      'prefers YAML value over env variable',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'env-fills-gaps',
        check: 'a-file-value-overrides-the-environment-variable',
      },
      () => {
        const EnvConfig = Config('env.test', {
          port: Env('TEST_PORT', Int),
        });

        process.env.TEST_PORT = '9090';
        process.env.CONFIG_DATA = JSON.stringify({
          env: { test: { port: 8080 } },
        });

        const config = useConfig(EnvConfig);
        expect(config.port).toBe(8080);
      },
    );
  });

  describe('Sensitive()', () => {
    specTest(
      'redacts sensitive fields in validation errors',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'secret-redaction',
        check: 'a-sensitive-field-is-redacted-in-validation-errors',
      },
      () => {
        const SecretConfig = Config('secret', {
          password: Sensitive(String),
        });

        process.env.CONFIG_DATA = JSON.stringify({});

        expect(() => useConfig(SecretConfig)).toThrow(/\[redacted\]/);
      },
    );
  });

  describe('resetConfigLoader', () => {
    it('clears the config cache', () => {
      const ResetConfig = Config('reset.test', {
        value: Default(String, 'default'),
      });

      process.env.CONFIG_DATA = JSON.stringify({
        reset: { test: { value: 'first' } },
      });

      const config1 = useConfig(ResetConfig);
      expect(config1.value).toBe('first');

      // Change config data
      process.env.CONFIG_DATA = JSON.stringify({
        reset: { test: { value: 'second' } },
      });

      resetConfigLoader();

      const config2 = useConfig(ResetConfig);
      expect(config2.value).toBe('second');
    });

    it('always clears cache even when globalConfig is undefined', () => {
      resetConfigLoader(); // First reset
      resetConfigLoader(); // Second reset should not throw
      expect(true).toBe(true);
    });
  });
});

describe('redactErrors', () => {
  specTest(
    'redacts top-level and nested sensitive fields without leaking values',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'secret-redaction',
      check: 'a-nested-sensitive-field-is-redacted-too',
    },
    () => {
      const schema = {
        apiKey: Sensitive(Default(String, '')),
        integrations: { stripe: { secret: Sensitive(Default(String, '')) } },
        host: Default(String, 'localhost'),
      };

      const out = redactErrors(
        [
          { field: 'apiKey', message: 'invalid: sk_top_secret' },
          { field: 'integrations.stripe.secret', message: 'invalid: sk_nested_secret' },
          { field: 'host', message: 'host is required' },
        ],
        schema,
      );

      expect(out).toContain('apiKey [redacted]');
      expect(out).toContain('integrations.stripe.secret [redacted]');
      expect(out).toContain('host is required');
      expect(out).not.toContain('sk_top_secret');
      expect(out).not.toContain('sk_nested_secret');
    },
  );

  it('strips the exact config label before resolving nested sensitive fields', () => {
    const schema = {
      integrations: { stripe: { secret: Sensitive(Default(String, '')) } },
    };

    const out = redactErrors(
      [{ field: 'integrations.integrations.stripe.secret', message: 'invalid: sk_overlap_secret' }],
      schema,
      'integrations',
    );

    expect(out).toContain('integrations.integrations.stripe.secret [redacted]');
    expect(out).not.toContain('sk_overlap_secret');
  });
});

describe('useRawConfigSection', () => {
  beforeEach(() => {
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  afterEach(() => {
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  it('reads a section of the merged tree without schema validation', () => {
    process.env.CONFIG_DATA = JSON.stringify({
      database: {
        operatorKey: 'keep-me',
        protocolVersion: 1,
        databases: { auth: { engine: 'postgres' } },
      },
    });

    const section = useRawConfigSection('database');
    expect(section?.['operatorKey']).toBe('keep-me');
    expect(section?.['protocolVersion']).toBe(1);
    expect(section?.['databases']).toEqual({ auth: { engine: 'postgres' } });
  });

  it('supports nested dot paths', () => {
    process.env.CONFIG_DATA = JSON.stringify({ database: { auth: { host: 'h' } } });
    expect(useRawConfigSection('database.auth')).toEqual({ host: 'h' });
  });

  it('returns undefined for an absent path', () => {
    process.env.CONFIG_DATA = JSON.stringify({ app: { name: 'x' } });
    expect(useRawConfigSection('database')).toBeUndefined();
  });

  it('returns undefined when the path does not hold an object', () => {
    process.env.CONFIG_DATA = JSON.stringify({ database: 'not-an-object', list: [1, 2] });
    expect(useRawConfigSection('database')).toBeUndefined();
    expect(useRawConfigSection('list')).toBeUndefined();
  });
});
