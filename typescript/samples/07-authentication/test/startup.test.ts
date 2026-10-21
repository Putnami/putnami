import { existsSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it } from 'bun:test';
import type { Application } from '@putnami/application';
import { useRawConfigSection } from '@putnami/runtime';
import { app } from '../src/main';
import { setOAuthCredentials } from './credentials';

const BUILD_OPTIONS = { publishCapabilityManifest: false, publishMigrationArtifacts: false };

/** The committed base configuration that reads the credentials from the environment. */
const COMMITTED_CONF = resolve(import.meta.dir, '..', 'conf', '.env.yaml');

/** The whole refusal when the client secret is usable and the client id is not. */
const ONLY_THE_CLIENT_ID_IS_MISSING = /^Missing required OAuth credentials: OAUTH_CLIENT_ID \(oauth\.clientId\)$/;

describe('authentication sample composition', () => {
  let restoreCredentials: (() => void) | undefined;
  let application: Application | undefined;

  afterEach(async () => {
    await application?.stop();
    application = undefined;
    restoreCredentials?.();
    restoreCredentials = undefined;
  });

  it('builds without OAuth credentials and reports its routes', async () => {
    restoreCredentials = setOAuthCredentials({});

    const result = await app().build(BUILD_OPTIONS);

    expect(result.httpRoutes).toContainEqual(
      expect.objectContaining({
        path: '/',
        provenance: expect.objectContaining({ sourceKind: 'manual' }),
      }),
    );
    expect(result.httpRoutes).toContainEqual(
      expect.objectContaining({
        path: '/auth/session',
        provenance: expect.objectContaining({ sourceKind: 'typed-api' }),
      }),
    );
  });

  it('refuses to start without OAuth credentials while the committed configuration is loaded', async () => {
    restoreCredentials = setOAuthCredentials({});
    // The committed conf/.env.yaml is present and loaded: it interpolates both
    // unset variables to empty values, which the start check refuses.
    expect(existsSync(COMMITTED_CONF)).toBe(true);
    expect(useRawConfigSection('oauth')).toEqual(expect.objectContaining({ clientId: '', clientSecret: '' }));
    application = app();
    await application.build(BUILD_OPTIONS);

    await expect(application.start()).rejects.toThrow(
      'Missing required OAuth credentials: OAUTH_CLIENT_ID (oauth.clientId), OAUTH_CLIENT_SECRET (oauth.clientSecret)',
    );
  });

  it('refuses to start when only the client secret is missing', async () => {
    restoreCredentials = setOAuthCredentials({ OAUTH_CLIENT_ID: 'test-client-id' });
    application = app();
    await application.build(BUILD_OPTIONS);

    await expect(application.start()).rejects.toThrow(
      'Missing required OAuth credentials: OAUTH_CLIENT_SECRET (oauth.clientSecret)',
    );
  });

  it('refuses to start when a credential is an unresolved reference to an unset variable', async () => {
    // A higher-priority source names the variable without a default. The loader
    // leaves the reference in place when the variable is unset, so the value is
    // a non-empty literal that must not pass as a credential.
    restoreCredentials = setOAuthCredentials(
      { OAUTH_CLIENT_SECRET: 'test-client-secret' },
      { configData: "oauth:\n  clientId: '${OAUTH_CLIENT_ID}'\n" },
    );
    expect(existsSync(COMMITTED_CONF)).toBe(true);
    expect(useRawConfigSection('oauth')).toEqual(
      expect.objectContaining({ clientId: '${OAUTH_CLIENT_ID}', clientSecret: 'test-client-secret' }),
    );
    application = app();
    await application.build(BUILD_OPTIONS);

    await expect(application.start()).rejects.toThrow(ONLY_THE_CLIENT_ID_IS_MISSING);
  });

  it('refuses to start when a credential is blank', async () => {
    restoreCredentials = setOAuthCredentials({ OAUTH_CLIENT_ID: '   ', OAUTH_CLIENT_SECRET: 'test-client-secret' });
    application = app();
    await application.build(BUILD_OPTIONS);

    await expect(application.start()).rejects.toThrow(ONLY_THE_CLIENT_ID_IS_MISSING);
  });

  it('refuses to start when a credential contains an unfinished interpolation', async () => {
    restoreCredentials = setOAuthCredentials({
      OAUTH_CLIENT_ID: 'prefix-${',
      OAUTH_CLIENT_SECRET: 'test-client-secret',
    });
    application = app();
    await application.build(BUILD_OPTIONS);

    await expect(application.start()).rejects.toThrow(ONLY_THE_CLIENT_ID_IS_MISSING);
  });
});
