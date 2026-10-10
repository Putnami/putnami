// biome-ignore-all lint/suspicious/noConsole: Captures JsonSink output written through console.log.
import { application, bootstrapServe } from '@putnami/application';
import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { resetDefaultLogger, resetLoggerConfig } from '@putnami/runtime';
import { RemoteConfigSource, resetRemoteConfigSourceCacheForTest } from '../src/runtime/remote-config-source';
import { setSyncFetchForTest } from '../src/runtime/sync-fetch';

class ProcessExitError extends Error {
  constructor(readonly code?: string | number | null) {
    super(`process.exit(${code})`);
  }
}

describe('bootstrapServe', () => {
  const originalConsoleLog = console.log;
  const originalExit = process.exit;
  const envNames = ['K_SERVICE', 'CONFIG_SERVER_URL', 'CONFIG_SERVER_TOKEN', 'APP_NAME', 'APP_ENV'] as const;
  let envSnapshot: Record<(typeof envNames)[number], string | undefined>;

  beforeEach(() => {
    envSnapshot = Object.fromEntries(envNames.map((name) => [name, process.env[name]])) as typeof envSnapshot;
    for (const name of envNames) {
      delete process.env[name];
    }
    resetLoggerConfig();
    resetDefaultLogger();
    resetRemoteConfigSourceCacheForTest();
    setSyncFetchForTest(undefined);
  });

  afterEach(() => {
    for (const name of envNames) {
      const value = envSnapshot[name];
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
    console.log = originalConsoleLog;
    process.exit = originalExit;
    resetLoggerConfig();
    resetDefaultLogger();
    resetRemoteConfigSourceCacheForTest();
    setSyncFetchForTest(undefined);
  });

  it('logs a construction-time config-server failure as one structured JSON error line', async () => {
    const lines: string[] = [];
    console.log = mock((line?: unknown) => {
      lines.push(String(line ?? ''));
    }) as unknown as typeof console.log;
    process.exit = mock((code?: string | number | null) => {
      throw new ProcessExitError(code);
    }) as unknown as typeof process.exit;

    process.env.K_SERVICE = 'runtime-startup-test';
    setSyncFetchForTest(() => ({ status: 404, body: 'not found' }));

    let exitCode: string | number | null | undefined;
    try {
      await bootstrapServe(() => {
        new RemoteConfigSource({
          serverUrl: 'https://config.putnami.test',
          appName: 'task-api',
          environment: 'prod',
          token: 'operator-token',
          required: true,
        }).load();
        return application();
      });
    } catch (error) {
      if (!(error instanceof ProcessExitError)) {
        throw error;
      }
      exitCode = error.code;
    }

    expect(exitCode).toBe(1);
    expect(lines).toHaveLength(1);
    const entry = JSON.parse(lines[0]) as Record<string, unknown>;
    expect(entry.severity).toBe('ERROR');
    expect(entry.message).toContain('startup failed: config-server returned non-200: status 404');
    expect(entry.error).toMatchObject({
      name: 'RemoteConfigError',
      message: 'config-server returned non-200: status 404',
      url: 'https://config.putnami.test/api/configs/resolve',
      method: 'POST',
      statusCode: 404,
      authPresent: true,
      authMode: 'bearer',
      appName: 'task-api',
      environment: 'prod',
      cause: {
        name: 'Error',
        message: 'status 404',
      },
    });
    expect((entry.error as { stack?: unknown }).stack).toEqual(expect.any(String));
  });
});
