import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Config } from '../../src/config/config';
import {
  buildInfraRequirements,
  collectSecretNames,
  emitInfraRequirements,
  type InfraRequirementsManifest,
} from '../../src/config/infra-requirements';
import { Default, Desc, Env, Int, Optional, Sensitive } from '../../src/schema';
import { specTest } from '../../src/spectest';

describe('collectSecretNames', () => {
  it('returns no secrets when no fields are sensitive', () => {
    const config = Config('server', { host: String, port: Default(Int, 8080) });
    expect(collectSecretNames([config])).toEqual([]);
  });

  it('collects a single sensitive field by its config path', () => {
    const config = Config('database', {
      host: Default(String, 'localhost'),
      password: Sensitive(Env('DB_PASSWORD', String)),
    });
    expect(collectSecretNames([config])).toEqual(['database.password']);
  });

  it('collects multiple sensitive fields, sorted and across config blocks', () => {
    const database = Config('database', {
      host: String,
      password: Sensitive(String),
    });
    const auth = Config('auth', {
      token: Sensitive(String),
      apikey: Sensitive(Env('API_KEY', String)),
    });
    expect(collectSecretNames([database, auth])).toEqual(['auth.apikey', 'auth.token', 'database.password']);
  });

  it('preserves the sensitive marker through Optional and Default wrappers', () => {
    const config = Config('svc', {
      a: Optional(Sensitive(String)),
      b: Sensitive(Optional(String)),
    });
    expect(collectSecretNames([config])).toEqual(['svc.a', 'svc.b']);
  });

  it('walks into nested plain-object schemas', () => {
    const config = Config('integrations', {
      stripe: { publishable: String, secret: Sensitive(String) },
    });
    expect(collectSecretNames([config])).toEqual(['integrations.stripe.secret']);
  });

  it('walks into Desc-wrapped nested objects', () => {
    const config = Config('oauth', {
      provider: Desc('OAuth provider settings', {
        clientId: String,
        clientSecret: Sensitive(String),
      }),
    });
    expect(collectSecretNames([config])).toEqual(['oauth.provider.client_secret']);
  });

  specTest(
    'canonicalizes camelCase config keys to the lowercase infra grammar',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'secret-names-only',
      check: 'secret-names-are-canonicalized-to-the-infra-grammar',
    },
    () => {
      const config = Config('integrations', {
        stripe: { publishable: String, apiKey: Sensitive(String), webhookSecret: Sensitive(String) },
      });
      expect(collectSecretNames([config])).toEqual([
        'integrations.stripe.api_key',
        'integrations.stripe.webhook_secret',
      ]);
    },
  );

  it('throws a clear error when a sensitive field cannot yield a valid infra name', () => {
    const config = Config('svc', { 'bad key': Sensitive(String) });
    expect(() => collectSecretNames([config])).toThrow(/infra resource-name grammar/);
  });

  it('treats a sensitive object as one secret and does not walk its children', () => {
    const config = Config('vault', {
      creds: Sensitive(Desc('opaque credentials blob', { user: String, pass: String })),
    });
    expect(collectSecretNames([config])).toEqual(['vault.creds']);
  });

  it('deduplicates identical secret paths declared across blocks', () => {
    const a = Config('shared', { token: Sensitive(String) });
    const b = Config('shared', { token: Sensitive(String) });
    expect(collectSecretNames([a, b])).toEqual(['shared.token']);
  });
});

describe('buildInfraRequirements', () => {
  it('returns undefined when there are no secrets', () => {
    const config = Config('server', { host: String });
    expect(buildInfraRequirements([config])).toBeUndefined();
  });

  specTest(
    'builds a v1 manifest with the schema reference and sorted secrets',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'secret-names-only',
      check: 'the-manifest-carries-sorted-secret-names',
    },
    () => {
      const config = Config('database', { password: Sensitive(String), apikey: Sensitive(String) });
      expect(buildInfraRequirements([config])).toEqual({
        $schema: 'https://putnami.dev/schemas/putnami-infra.json',
        protocolVersion: 2,
        secrets: ['database.apikey', 'database.password'],
      });
    },
  );
});

describe('emitInfraRequirements', () => {
  let projectDir: string;
  const sidecarPath = (root: string) => join(root, '.gen', 'infra', 'secrets.json');

  beforeEach(() => {
    projectDir = mkdtempSync(join(tmpdir(), 'putnami-infra-'));
  });

  afterEach(() => {
    rmSync(projectDir, { recursive: true, force: true });
  });

  it('writes the sidecar manifest when secrets are declared', () => {
    const config = Config('database', { password: Sensitive(String) });
    const written = emitInfraRequirements(projectDir, [config]);

    expect(written).toBe(sidecarPath(projectDir));
    const manifest = JSON.parse(readFileSync(written!, 'utf-8')) as InfraRequirementsManifest;
    expect(manifest).toEqual({
      $schema: 'https://putnami.dev/schemas/putnami-infra.json',
      protocolVersion: 2,
      secrets: ['database.password'],
    });
  });

  it('terminates the file with a trailing newline', () => {
    const config = Config('database', { password: Sensitive(String) });
    const written = emitInfraRequirements(projectDir, [config]);
    expect(readFileSync(written!, 'utf-8').endsWith('}\n')).toBe(true);
  });

  it('writes no file when no sensitive fields are declared', () => {
    const config = Config('server', { host: String });
    expect(emitInfraRequirements(projectDir, [config])).toBeUndefined();
    expect(existsSync(sidecarPath(projectDir))).toBe(false);
  });

  specTest(
    'removes a stale sidecar when secrets are no longer declared',
    {
      feature: 'typescript/typed-configuration',
      requirement: 'secret-names-only',
      check: 'a-stale-sidecar-is-removed-when-no-secret-remains',
    },
    () => {
      const withSecret = Config('database', { password: Sensitive(String) });
      const path = emitInfraRequirements(projectDir, [withSecret]);
      expect(existsSync(path!)).toBe(true);

      const withoutSecret = Config('database', { host: String });
      expect(emitInfraRequirements(projectDir, [withoutSecret])).toBeUndefined();
      expect(existsSync(sidecarPath(projectDir))).toBe(false);
    },
  );

  it('overwrites an existing sidecar with the current secrets', () => {
    const first = Config('database', { password: Sensitive(String) });
    emitInfraRequirements(projectDir, [first]);

    const second = Config('database', { token: Sensitive(String) });
    const path = emitInfraRequirements(projectDir, [second]);
    const manifest = JSON.parse(readFileSync(path!, 'utf-8')) as InfraRequirementsManifest;
    expect(manifest.secrets).toEqual(['database.token']);
  });

  it('treats a missing stale sidecar as a no-op', () => {
    const config = Config('server', { host: String });
    expect(() => emitInfraRequirements(projectDir, [config])).not.toThrow();
  });

  it('leaves no temp file behind after an atomic write', () => {
    const config = Config('database', { password: Sensitive(String) });
    emitInfraRequirements(projectDir, [config]);
    const leftovers = readdirSync(join(projectDir, '.gen', 'infra')).filter((f) => f.endsWith('.tmp'));
    expect(leftovers).toEqual([]);
  });

  it('uses a unique temp name so repeated writers do not race on rename', () => {
    const config = Config('database', { password: Sensitive(String) });
    const run = () => {
      for (let i = 0; i < 8; i++) {
        emitInfraRequirements(projectDir, [config]);
      }
    };
    expect(run).not.toThrow();
    const manifest = JSON.parse(readFileSync(sidecarPath(projectDir), 'utf-8')) as InfraRequirementsManifest;
    expect(manifest.secrets).toEqual(['database.password']);
  });
});
