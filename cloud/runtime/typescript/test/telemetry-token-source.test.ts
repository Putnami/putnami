import { afterEach, describe, expect } from 'bun:test';
import { getTelemetryTokenSource } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { resetTelemetryTokenSourceForTest } from '@putnami/runtime/testing';
import { cloudConfigLoaderResetHooks, register } from '../src/runtime/register';
import {
  cloudTelemetryTokenSource,
  googleTelemetryTokenSource,
  resetTelemetryTokenCacheForTest,
} from '../src/runtime/telemetry-token-source';

const FEATURE = 'cloud/typescript-runtime-destination';
const REQUIREMENT = 'register-authenticates-telemetry';
const COLLECTOR = 'https://ws-otel-collector-abc.a.run.app';
const NOW_MS = Date.UTC(2026, 10, 16, 12);

function jwt(audience: string, expiresAtMs: number): string {
  const header = Buffer.from(JSON.stringify({ alg: 'RS256', typ: 'JWT' })).toString('base64url');
  const payload = Buffer.from(JSON.stringify({ aud: audience, exp: Math.floor(expiresAtMs / 1000) })).toString(
    'base64url',
  );
  return `${header}.${payload}.signature`;
}

/** A metadata server that answers an ID token for the requested audience. */
function metadataServer() {
  const asked: string[] = [];
  const fetch = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = new URL(input instanceof Request ? input.url : input.toString());
    expect(new Headers(init?.headers).get('Metadata-Flavor')).toBe('Google');
    const audience = url.searchParams.get('audience') ?? '';
    asked.push(audience);
    return new Response(jwt(audience, NOW_MS + 60 * 60 * 1000), { headers: { 'Metadata-Flavor': 'Google' } });
  };
  return { asked, fetch };
}

describe('telemetry token source', () => {
  afterEach(() => {
    resetTelemetryTokenSourceForTest();
  });

  specTest(
    'register registers the Cloud telemetry token source',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'register-registers-the-telemetry-token-source' },
    () => {
      register({ registerSourceDiscoverer: () => {} });
      expect(getTelemetryTokenSource()).toBe(cloudTelemetryTokenSource);
      register({ registerSourceDiscoverer: () => {} });
      expect(getTelemetryTokenSource()).toBe(cloudTelemetryTokenSource);
      expect(cloudConfigLoaderResetHooks).toContain(resetTelemetryTokenCacheForTest);
    },
  );

  specTest(
    'on Google Cloud a push gets an ID token for the collector, cached until it nears expiry',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'on-google-cloud-a-push-carries-an-id-token-for-the-collector',
    },
    async () => {
      const metadata = metadataServer();
      const source = googleTelemetryTokenSource({
        env: { K_SERVICE: 'site' },
        fetch: metadata.fetch,
        now: () => NOW_MS,
      });

      const first = await source(COLLECTOR);
      const second = await source(COLLECTOR);

      expect(first).toBe(jwt(COLLECTOR, NOW_MS + 60 * 60 * 1000));
      expect(second).toBe(first);
      expect(metadata.asked).toEqual([COLLECTOR]);

      const project = googleTelemetryTokenSource({
        env: { GOOGLE_CLOUD_PROJECT: 'p' },
        fetch: metadata.fetch,
        now: () => NOW_MS,
      });
      expect(await project(COLLECTOR)).toBe(first);
    },
  );

  specTest(
    'a wrong-audience token is rejected',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'on-google-cloud-a-push-carries-an-id-token-for-the-collector',
    },
    async () => {
      const source = googleTelemetryTokenSource({
        env: { K_SERVICE: 'site' },
        now: () => NOW_MS,
        fetch: async () =>
          new Response(jwt('https://elsewhere.example', NOW_MS + 60 * 60 * 1000), {
            headers: { 'Metadata-Flavor': 'Google' },
          }),
      });
      await expect(source(COLLECTOR)).rejects.toThrow();
    },
  );

  specTest(
    'off Google Cloud no token is supplied and the metadata server is never asked',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'off-google-cloud-no-token-is-supplied' },
    async () => {
      const metadata = metadataServer();
      const source = googleTelemetryTokenSource({ env: {}, fetch: metadata.fetch, now: () => NOW_MS });
      expect(await source(COLLECTOR)).toBeUndefined();
      expect(metadata.asked).toEqual([]);
    },
  );
});
