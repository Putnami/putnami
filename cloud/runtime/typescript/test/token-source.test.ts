// biome-ignore-all lint/suspicious/noConsole: Captures JsonSink output written through console.log.
// biome-ignore-all lint/performance/noDelete: `delete process.env.X` is the canonical way to unset an env var; tests need that exact semantic.
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import {
  CONFIG_SERVER_TOKEN_ENV_NAMES,
  discoverTokenSource,
  EnvVarTokenSource,
  GcpMetadataTokenSource,
  parseJWTExpiry,
} from '../src/runtime/token-source';
import { setSyncFetchForTest, type SyncFetchRequest } from '../src/runtime/sync-fetch';
import { resetDefaultLogger } from '@putnami/runtime';
import { resetLoggerConfig } from '@putnami/runtime';

function jwtWithExp(exp: number): string {
  const header = Buffer.from('{"alg":"RS256","typ":"JWT"}').toString('base64url');
  const payload = Buffer.from(JSON.stringify({ exp })).toString('base64url');
  return `${header}.${payload}.signature`;
}

describe('EnvVarTokenSource', () => {
  afterEach(() => {
    delete process.env.CONFIG_TEST_TOKEN;
  });

  it('returns the env value, trimmed', () => {
    process.env.CONFIG_TEST_TOKEN = '  abc123  ';
    expect(new EnvVarTokenSource('CONFIG_TEST_TOKEN').token()).toBe('abc123');
  });

  it('returns undefined when env is unset', () => {
    delete process.env.CONFIG_TEST_TOKEN;
    expect(new EnvVarTokenSource('CONFIG_TEST_TOKEN').token()).toBeUndefined();
  });

  it('returns undefined when env is empty whitespace', () => {
    process.env.CONFIG_TEST_TOKEN = '   ';
    expect(new EnvVarTokenSource('CONFIG_TEST_TOKEN').token()).toBeUndefined();
  });
});

describe('GcpMetadataTokenSource', () => {
  beforeEach(() => {
    setSyncFetchForTest(undefined);
    resetLoggerConfig();
    resetDefaultLogger();
  });

  afterEach(() => {
    setSyncFetchForTest(undefined);
    resetLoggerConfig();
    resetDefaultLogger();
  });

  it('fetches the token via in-process sync fetch', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: jwtWithExp(Math.floor(Date.now() / 1000) + 3600),
      };
    });

    const src = new GcpMetadataTokenSource('https://control.putnami.test');
    const tok = src.token();

    expect(tok).toBeDefined();
    expect(captured?.url).toContain('metadata.google.internal');
    expect(captured?.url).toContain('audience=https%3A%2F%2Fcontrol.putnami.test');
    expect(captured?.timeoutMs).toBe(2000);
  });

  it('caches the token within TTL', () => {
    let calls = 0;
    setSyncFetchForTest(() => {
      calls += 1;
      return {
        status: 200,
        body: jwtWithExp(Math.floor(Date.now() / 1000) + 3600),
      };
    });

    const src = new GcpMetadataTokenSource('https://control.putnami.test');
    src.token();
    src.token();
    expect(calls).toBe(1);
  });

  it('returns undefined when metadata fetch fails', () => {
    setSyncFetchForTest(() => {
      throw new Error('metadata unavailable');
    });

    const src = new GcpMetadataTokenSource('https://control.putnami.test');
    expect(src.token()).toBeUndefined();
  });
});

describe('parseJWTExpiry', () => {
  it('extracts the exp claim', () => {
    const exp = Math.floor(Date.now() / 1000) + 3600;
    expect(parseJWTExpiry(jwtWithExp(exp))).toBe(exp);
  });

  it('returns 0 for a malformed token', () => {
    expect(parseJWTExpiry('not-a-jwt')).toBe(0);
  });

  it('returns 0 when exp is missing', () => {
    const header = Buffer.from('{}').toString('base64url');
    const payload = Buffer.from('{}').toString('base64url');
    expect(parseJWTExpiry(`${header}.${payload}.sig`)).toBe(0);
  });
});

const DISCOVERY_ENV_NAMES = [
  ...CONFIG_SERVER_TOKEN_ENV_NAMES,
  'CONFIG_SERVER_AUDIENCE',
  'K_SERVICE',
  'GOOGLE_CLOUD_PROJECT',
] as const;

/**
 * Restores discovery-relevant env vars to their pre-test values.
 * Captured per-test so a self-hosted GCP runner (where K_SERVICE /
 * GOOGLE_CLOUD_PROJECT are set in the shell) doesn't leak into other
 * tests.
 */
function snapshotEnv(): Record<string, string | undefined> {
  const snap: Record<string, string | undefined> = {};
  for (const name of DISCOVERY_ENV_NAMES) {
    snap[name] = process.env[name];
  }
  return snap;
}

function restoreEnv(snap: Record<string, string | undefined>) {
  for (const name of DISCOVERY_ENV_NAMES) {
    const v = snap[name];
    if (v === undefined) delete process.env[name];
    else process.env[name] = v;
  }
}

/**
 * Clears every signal discoverTokenSource looks at so tests start from
 * "off-GCP, no operator override." Tests then explicitly set whichever
 * signal they want to assert on. Without this, a self-hosted GCP
 * runner would flip discovery silently (K_SERVICE inherited from the
 * shell) and tests would pass for the wrong reason or fail outright.
 */
function isolateDiscoveryEnv() {
  for (const name of DISCOVERY_ENV_NAMES) {
    delete process.env[name];
  }
}

describe('discoverTokenSource', () => {
  let snap: ReturnType<typeof snapshotEnv>;

  beforeEach(() => {
    snap = snapshotEnv();
    isolateDiscoveryEnv();
    resetLoggerConfig();
    resetDefaultLogger();
  });

  afterEach(() => {
    restoreEnv(snap);
    // A test below installs a sync-fetch mock; leaving it installed answers
    // every later syncFetch in this process (another test file included).
    setSyncFetchForTest(undefined);
    resetLoggerConfig();
    resetDefaultLogger();
  });

  it('prefers CONFIG_SERVER_TOKEN over GCP workload identity', () => {
    // An operator override always wins.
    process.env.CONFIG_SERVER_TOKEN = 'operator-override';
    process.env.K_SERVICE = 'task-api'; // pretend Cloud Run
    const src = discoverTokenSource('https://control.putnami.test');
    expect(src).toBeInstanceOf(EnvVarTokenSource);
  });

  it('picks GcpMetadataTokenSource when K_SERVICE is set and CONFIG_SERVER_TOKEN is unset', () => {
    process.env.K_SERVICE = 'task-api';
    const src = discoverTokenSource('https://control.putnami.test');
    expect(src).toBeInstanceOf(GcpMetadataTokenSource);
  });

  it('uses CONFIG_SERVER_AUDIENCE for GCP metadata identity when set', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: jwtWithExp(Math.floor(Date.now() / 1000) + 3600),
      };
    });
    process.env.K_SERVICE = 'task-api';
    process.env.CONFIG_SERVER_AUDIENCE = 'https://api.putnami.test';
    const src = discoverTokenSource(
      'https://api.putnami.test/api/configs/resolve?appName=auth%2Fserver&environment=prod&secretsMode=reveal',
    );
    expect(src).toBeInstanceOf(GcpMetadataTokenSource);
    src.token();
    expect(captured?.url).toContain('audience=https%3A%2F%2Fapi.putnami.test');
  });

  it('picks GcpMetadataTokenSource when GOOGLE_CLOUD_PROJECT is set', () => {
    process.env.GOOGLE_CLOUD_PROJECT = 'my-project';
    const src = discoverTokenSource('https://control.putnami.test');
    expect(src).toBeInstanceOf(GcpMetadataTokenSource);
  });

  it('falls back to EnvVarTokenSource off-GCP', () => {
    // isolateDiscoveryEnv already cleared every signal.
    const src = discoverTokenSource('https://control.putnami.test');
    expect(src).toBeInstanceOf(EnvVarTokenSource);
  });

  // Precedence: PUTNAMI_CLOUD_TOKEN > CONFIG_SERVER_TOKEN > PUTNAMI_TOKEN.
  // Most specific name wins so workspace setup remains predictable when
  // operators set more than one (intentionally during migration, or
  // unintentionally via leftover shell config).
  const precedenceCases: Array<{
    name: string;
    env: Partial<Record<(typeof CONFIG_SERVER_TOKEN_ENV_NAMES)[number], string>>;
    want: string;
  }> = [
    {
      name: 'PUTNAMI_CLOUD_TOKEN wins over CONFIG_SERVER_TOKEN',
      env: { PUTNAMI_CLOUD_TOKEN: 'cloud', CONFIG_SERVER_TOKEN: 'config' },
      want: 'cloud',
    },
    {
      name: 'CONFIG_SERVER_TOKEN wins over PUTNAMI_TOKEN',
      env: { CONFIG_SERVER_TOKEN: 'config', PUTNAMI_TOKEN: 'putnami' },
      want: 'config',
    },
    {
      name: 'PUTNAMI_TOKEN used when only one set',
      env: { PUTNAMI_TOKEN: 'putnami' },
      want: 'putnami',
    },
    {
      name: 'PUTNAMI_CLOUD_TOKEN beats all three',
      env: {
        PUTNAMI_CLOUD_TOKEN: 'cloud',
        CONFIG_SERVER_TOKEN: 'config',
        PUTNAMI_TOKEN: 'putnami',
      },
      want: 'cloud',
    },
  ];

  for (const tc of precedenceCases) {
    it(`env-var precedence: ${tc.name}`, () => {
      // Force K_SERVICE so we also prove env-var wins over GCP.
      process.env.K_SERVICE = 'task-api';
      for (const [k, v] of Object.entries(tc.env)) {
        process.env[k] = v;
      }
      const src = discoverTokenSource('https://control.putnami.test');
      expect(src).toBeInstanceOf(EnvVarTokenSource);
      expect(src.token()).toBe(tc.want);
    });
  }
});
