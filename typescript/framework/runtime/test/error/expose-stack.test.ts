import { describe, expect, it } from 'bun:test';
import { restoreEnv } from '@putnami/utils';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { shouldExposeErrorStack } from '../../src/error/expose-stack';

const SOURCE = join(import.meta.dir, '..', '..', 'src', 'error', 'expose-stack.ts');

/** Runs `fn` with a fixed NODE_ENV / K_SERVICE pair, then restores both. */
const withEnv = <T>(nodeEnv: string | undefined, service: string | undefined, fn: () => T): T => {
  const previousNodeEnv = process.env.NODE_ENV;
  const previousService = process.env.K_SERVICE;
  restoreEnv('NODE_ENV', nodeEnv);
  restoreEnv('K_SERVICE', service);
  try {
    return fn();
  } finally {
    restoreEnv('NODE_ENV', previousNodeEnv);
    restoreEnv('K_SERVICE', previousService);
  }
};

describe('shouldExposeErrorStack', () => {
  it('hides stacks in every production shape and exposes them elsewhere', () => {
    expect(withEnv('production', undefined, shouldExposeErrorStack)).toBe(false);
    expect(withEnv('prod', undefined, shouldExposeErrorStack)).toBe(false);
    // Cloud Run sets K_SERVICE even when the image forgot NODE_ENV.
    expect(withEnv('development', 'my-service', shouldExposeErrorStack)).toBe(false);
    expect(withEnv(undefined, 'my-service', shouldExposeErrorStack)).toBe(false);

    expect(withEnv('development', undefined, shouldExposeErrorStack)).toBe(true);
    expect(withEnv('test', undefined, shouldExposeErrorStack)).toBe(true);
    expect(withEnv(undefined, undefined, shouldExposeErrorStack)).toBe(true);
  });

  it('hides when there is no process at all, as in a browser bundle', () => {
    // A browser has no environment to read, so it cannot assert that it is a
    // development environment — and a guard that cannot tell must assume
    // production. `@putnami/web` re-opens this case in development through the
    // flag SSR injects; nothing here may open it by default.
    const previous = Object.getOwnPropertyDescriptor(globalThis, 'process');
    // biome-ignore lint/performance/noDelete: restoring the own property descriptor requires deleting it first.
    delete (globalThis as { process?: unknown }).process;
    try {
      // Non-vacuity: the probe means nothing if the global survived the delete.
      expect((globalThis as { process?: unknown }).process).toBeUndefined();
      expect(shouldExposeErrorStack()).toBe(false);
    } finally {
      if (previous) Object.defineProperty(globalThis, 'process', previous);
    }
  });

  it('hides when process carries no env, as a partial browser shim does', () => {
    // A bundler shim that defines `process` but leaves `env` empty must not
    // read as development either.
    const previous = Object.getOwnPropertyDescriptor(globalThis, 'process');
    Object.defineProperty(globalThis, 'process', { value: {}, configurable: true, writable: true });
    try {
      expect(shouldExposeErrorStack()).toBe(false);
    } finally {
      if (previous) Object.defineProperty(globalThis, 'process', previous);
    }
  });

  /**
   * The guard's whole point is surviving publication. `bun build` substitutes
   * the literal member expression `process.env.NODE_ENV` with its build-time
   * value on every target, so the previous implementation shipped as a literal
   * `true` and no consumer environment could close it. These builds run with
   * the test process' NODE_ENV (never `production`), then the assertions read
   * the emitted bytes and call the emitted function with NODE_ENV flipped.
   */
  for (const target of ['browser', 'bun', 'node'] as const) {
    for (const minify of [false, true]) {
      it(`survives bundling for target ${target}${minify ? ' (minified)' : ''}`, async () => {
        const outputRoot = mkdtempSync(join(tmpdir(), 'putnami-expose-stack-'));
        try {
          const built = await Bun.build({
            entrypoints: [SOURCE],
            target,
            format: 'esm',
            outdir: outputRoot,
            // Publication minifies by default; the minifier must not fold the
            // read back into a literal either.
            minify,
          });
          if (!built.success) {
            throw new Error(built.logs.map((log) => log.message).join('\n'));
          }
          const entryOutput = built.outputs.find((output) => output.kind === 'entry-point');
          if (!entryOutput) throw new Error('no entry-point emitted');

          // A folded guard drops the variable name entirely: `NODE_ENV` only
          // survives in the artifact when the lookup happens at runtime.
          const source = await entryOutput.text();
          expect(source).toContain('NODE_ENV');
          expect(source).toContain('K_SERVICE');

          const bundled = (await import(entryOutput.path)) as { shouldExposeErrorStack: () => boolean };
          expect(withEnv('production', undefined, bundled.shouldExposeErrorStack)).toBe(false);
          expect(withEnv('development', 'my-service', bundled.shouldExposeErrorStack)).toBe(false);
          // Liveness: proves the emitted read is dynamic in both directions.
          expect(withEnv('development', undefined, bundled.shouldExposeErrorStack)).toBe(true);
        } finally {
          rmSync(outputRoot, { recursive: true, force: true });
        }
        // Generous: each case spawns a bundler, and six of them share a machine
        // with the rest of the gate.
      }, 60_000);
    }
  }
});
