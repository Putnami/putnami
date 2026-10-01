import { describe, expect, it } from 'bun:test';
import { shouldExposeErrorStack } from '@putnami/runtime';
import { restoreEnv } from '@putnami/utils';
import { shouldExposeClientErrors } from '../../src/client/error/expose-errors';

/**
 * `shouldExposeClientErrors()` deliberately re-implements the environment half
 * of `@putnami/runtime`'s `shouldExposeErrorStack()` instead of calling it: the
 * runtime root barrel re-exports the filesystem-backed config loader, and
 * `src/client/**` is the published browser graph. This file is the seam that
 * keeps the copy honest — it may import the runtime freely, because a test is
 * not in that graph.
 */

const ENV_MATRIX: [nodeEnv: string | undefined, service: string | undefined][] = [
  ['production', undefined],
  ['prod', undefined],
  ['development', undefined],
  ['test', undefined],
  [undefined, undefined],
  ['development', 'my-service'],
  [undefined, 'my-service'],
  ['production', 'my-service'],
];

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

describe('shouldExposeClientErrors', () => {
  it('agrees with shouldExposeErrorStack on every environment, with no window', () => {
    // Non-vacuity: the matrix must exercise both answers, or an implementation
    // returning a constant would satisfy the comparison.
    const answers = ENV_MATRIX.map(([nodeEnv, service]) => withEnv(nodeEnv, service, shouldExposeErrorStack));
    expect(new Set(answers)).toEqual(new Set([true, false]));

    for (const [nodeEnv, service] of ENV_MATRIX) {
      expect([nodeEnv, service, withEnv(nodeEnv, service, shouldExposeClientErrors)]).toEqual([
        nodeEnv,
        service,
        withEnv(nodeEnv, service, shouldExposeErrorStack),
      ]);
    }
  });

  it('imports nothing: the published browser graph must not reach the runtime barrel', async () => {
    // The regression this guards: importing `@putnami/runtime` here drags
    // `./config` — node:fs, node:module, the filesystem config sources — into
    // the browser bundle, which is exactly what `expose-stack.ts` documents it
    // stays dependency-free to avoid.
    const source = await Bun.file(new URL('../../src/client/error/expose-errors.ts', import.meta.url).pathname).text();
    expect(source).not.toContain("from '@putnami/");
  });

  it('consults only the injected flag once a window exists', () => {
    const previousWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
    const install = (value: unknown) =>
      Object.defineProperty(globalThis, 'window', { value, configurable: true, writable: true });
    try {
      // A development environment must not open the browser branch: only the
      // server's flag may.
      withEnv('development', undefined, () => {
        install({});
        expect(shouldExposeClientErrors()).toBe(false);
        install({ __putnamiExposeErrors: false });
        expect(shouldExposeClientErrors()).toBe(false);
        install({ __putnamiExposeErrors: true });
        expect(shouldExposeClientErrors()).toBe(true);
      });
    } finally {
      if (previousWindow) Object.defineProperty(globalThis, 'window', previousWindow);
      else (globalThis as { window?: unknown }).window = undefined;
    }
  });
});
