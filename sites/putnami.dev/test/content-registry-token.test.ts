/**
 * registry-token/v1 child-process tests.
 *
 * These run a real child process in place of `putnami cloud registry-token`:
 * the Bun executable with an inline script. The child talks to a local
 * Bun.serve server; no network, no real registry, no real credential.
 *
 * The non-blocking test uses an explicit barrier instead of a sleep: the child
 * waits on a `/gate` request that this process answers only when the test
 * releases it. A blocking spawn would starve that server and every other
 * request, so the test cannot pass with one.
 *
 * Every test awaits its pending run before it returns, assertion failure or
 * not: a run stops and reaps its child before it settles, so no child outlives
 * its test.
 */
import { describe, expect, it } from 'bun:test';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  type ChannelPointer,
  type CommandRunner,
  fetchChannelPointerFromRegistry,
  NO_AUTO_INSTALL_ENV,
  resolveRegistryToken,
  type SpawnRunnerOptions,
  spawnCommandRunner,
} from '../src/lib/content/registry';
import { sha256Hex } from '../src/lib/content/verify';

const HOST = 'put.putnami.dev';

function deferred<T = void>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/** Waits for `promise` to settle and ignores its outcome. */
async function settle(promise: Promise<unknown> | undefined): Promise<void> {
  await promise?.then(
    () => undefined,
    () => undefined,
  );
}

/**
 * Child source that writes `text` to stdout. `JSON.stringify` yields the
 * string literal, so a script never carries a hand-written escape: the
 * child writes exactly `text`.
 */
function writeStdout(text: string): string {
  return `process.stdout.write(${JSON.stringify(text)});`;
}

/**
 * A runner that executes `script` with Bun in place of the putnami CLI, through
 * the real spawn runner. It records the seam argv it received.
 */
function bunScriptRunner(script: string, seen: string[][] = [], options?: SpawnRunnerOptions): CommandRunner {
  const real = spawnCommandRunner(options);
  return (argv) => {
    seen.push(argv);
    return real([process.execPath, '-e', script]);
  };
}

/** Sets `name` to `value` (or removes it when `value` is undefined). */
function setEnv(name: string, value: string | undefined): void {
  if (value === undefined) Reflect.deleteProperty(process.env, name);
  else process.env[name] = value;
}

describe('registry-token child process', () => {
  it('keeps serving other HTTP requests while a delayed token child runs, then sends its bearer', async () => {
    const gateReached = deferred();
    const gate = deferred();
    const digest = sha256Hex(new TextEncoder().encode('blob'));
    let pointerAuthorization: string | null | undefined;
    let pending: Promise<ChannelPointer> | undefined;

    const server = Bun.serve({
      hostname: '127.0.0.1',
      port: 0,
      async fetch(request) {
        const { pathname } = new URL(request.url);
        if (pathname === '/gate') {
          gateReached.resolve();
          await gate.promise;
          return new Response('go');
        }
        if (pathname === '/ping') return new Response('pong');
        if (pathname === '/site-content/cloud/channels/stable') {
          pointerAuthorization = request.headers.get('authorization');
          return Response.json({ version: '1.2.3', digest });
        }
        return new Response('not found', { status: 404 });
      },
    });
    try {
      const gateUrl = new URL('/gate', server.url).href;
      // The child blocks on the barrier, then prints its bearer. Its own
      // abort bounds it if the parent can never answer.
      const script =
        '(async () => {' +
        ' const signal = AbortSignal.timeout(10000);' +
        ` const r = await fetch(${JSON.stringify(gateUrl)}, { signal }).catch(() => null);` +
        ' if (!r) process.exit(2);' +
        ` ${writeStdout('tok-delayed\n')}` +
        '})();';
      const seen: string[][] = [];

      pending = fetchChannelPointerFromRegistry(
        'site-content/cloud',
        'stable',
        server.url.href,
        bunScriptRunner(script, seen, { timeoutMs: 15_000 }),
      );
      let settled = false;
      pending.then(
        () => {
          settled = true;
        },
        () => {
          settled = true;
        },
      );

      // The child is running and waits on this process. A run that ends
      // without reaching the gate ends this wait too, and fails below.
      await Promise.race([gateReached.promise, settle(pending)]);
      // Another real request completes before the token resolves.
      const ping = await fetch(new URL('/ping', server.url));
      expect(await ping.text()).toBe('pong');
      expect(settled).toBe(false);
      expect(pointerAuthorization).toBeUndefined();

      gate.resolve();
      const pointer = await pending;
      expect(pointer).toEqual({ version: '1.2.3', digest });
      expect(pointerAuthorization).toBe('Bearer tok-delayed');
      // argv form: the host is one discrete argument, no shell.
      expect(seen).toEqual([['putnami', 'cloud', 'registry-token', '--host', server.url.host]]);
    } finally {
      gate.resolve();
      // The server stays up until the run, its child and its pointer fetch end.
      await settle(pending);
      server.stop(true);
    }
  }, 30_000);

  it('maps every real-child failure mode to anonymous and accepts a bare bearer', async () => {
    const tokenFor = (script: string, options?: SpawnRunnerOptions) =>
      resolveRegistryToken(HOST, bunScriptRunner(script, [], options));

    expect(await tokenFor(writeStdout('tok-real\n'))).toBe('tok-real');
    // A non-zero exit discards stdout, even a valid-looking bearer.
    expect(await tokenFor(`${writeStdout('tok-real\n')} process.exit(3);`)).toBeUndefined();
    expect(await tokenFor('process.exit(0);')).toBeUndefined();
    // A status line is not a bearer (protocols/registry ValidBearer).
    expect(await tokenFor(writeStdout('not signed in\n'))).toBeUndefined();
    // Stdout past the cap stops a child that would otherwise run until the
    // deadline, and the run fails at once instead of at the deadline. The
    // deadline stays below the test timeout, so a regression still ends,
    // and stops its child, inside this test.
    const overflowStart = performance.now();
    expect(
      await tokenFor(`process.stdout.write('a'.repeat(4096)); setInterval(() => {}, 1000);`, {
        maxStdoutBytes: 1024,
        timeoutMs: 20_000,
      }),
    ).toBeUndefined();
    expect(performance.now() - overflowStart).toBeLessThan(10_000);
    // A child that fills stderr past a pipe buffer still delivers its bearer:
    // stderr is drained, so the child never blocks on it.
    expect(await tokenFor(`process.stderr.write('x'.repeat(1 << 20)); ${writeStdout('tok-after-stderr\n')}`)).toBe(
      'tok-after-stderr',
    );

    // A missing CLI is anonymous, not an error.
    const missing = spawnCommandRunner();
    expect(
      await resolveRegistryToken(HOST, (argv) => missing([join(tmpdir(), 'no-such-putnami-cli'), ...argv.slice(1)])),
    ).toBeUndefined();
  }, 30_000);

  it('gives the child PUTNAMI_NO_AUTO_INSTALL=1 on top of the inherited environment', async () => {
    const marker = 'REGISTRY_TOKEN_TEST_INHERITED';
    const savedMarker = process.env[marker];
    const savedNoAutoInstall = process.env[NO_AUTO_INSTALL_ENV];
    setEnv(marker, 'kept');
    setEnv(NO_AUTO_INSTALL_ENV, '0');
    try {
      // The child reports whether each value matched, never a value itself.
      const script =
        `const [set, inherited] = ${JSON.stringify([NO_AUTO_INSTALL_ENV, marker])};` +
        `const ok = process.env[set] === '1' && process.env[inherited] === 'kept';` +
        `if (ok) { ${writeStdout('tok-env\n')} } else { ${writeStdout('env-mismatch\n')} }`;
      expect(await resolveRegistryToken(HOST, bunScriptRunner(script))).toBe('tok-env');
    } finally {
      setEnv(marker, savedMarker);
      setEnv(NO_AUTO_INSTALL_ENV, savedNoAutoInstall);
    }
  }, 30_000);

  it('stops a timed-out child that ignores SIGTERM and falls back to anonymous', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'registry-token-'));
    const pidFile = join(dir, 'pid');
    const partial = JSON.stringify(`${pidFile}.partial`);
    const complete = JSON.stringify(pidFile);
    // The rename publishes the pid file only once it holds the whole pid.
    const script =
      `process.on('SIGTERM', () => {});` +
      `const fs = require('node:fs');` +
      `fs.writeFileSync(${partial}, String(process.pid));` +
      `fs.renameSync(${partial}, ${complete});` +
      'setInterval(() => {}, 1000);';

    const runner = bunScriptRunner(script, [], { timeoutMs: 3000, killGraceMs: 200 });
    let settled = false;
    const result = resolveRegistryToken(HOST, runner).finally(() => {
      settled = true;
    });
    try {
      // Wait for the child to start; the event loop stays free meanwhile.
      while (!existsSync(pidFile) && !settled) await Bun.sleep(10);
      expect(existsSync(pidFile)).toBe(true);
      const pid = Number.parseInt(readFileSync(pidFile, 'utf8'), 10);
      expect(pid).toBeGreaterThan(0);

      expect(await result).toBeUndefined();
      // The child is gone: SIGTERM was ignored, SIGKILL ended it, and it was reaped.
      expect(() => process.kill(pid, 0)).toThrow();
    } finally {
      await settle(result);
      rmSync(dir, { recursive: true, force: true });
    }
  }, 30_000);
});
