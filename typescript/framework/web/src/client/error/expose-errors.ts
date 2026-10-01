declare global {
  interface Window {
    /**
     * Set to `true` by SSR when the server decided to expose error diagnostics.
     * Absent means "do not expose" — see {@link shouldExposeClientErrors}.
     */
    __putnamiExposeErrors?: boolean;
  }
}

/**
 * Reads an environment variable through an indirection bundlers cannot
 * constant-fold. `bun build` substitutes the literal member expression
 * `process.env.NODE_ENV` with its build-time value on every target.
 */
function readRuntimeEnv(name: string): string | undefined {
  const runtime = globalThis as { process?: { env?: Record<string, string | undefined> } };
  return runtime.process?.env?.[name];
}

/**
 * Whether the client error boundaries may render `error.message` and
 * `error.stack`.
 *
 * A browser cannot work this out on its own. There is no `NODE_ENV` to read,
 * and writing the check inline would not help either: the bundler folds
 * `process.env.NODE_ENV` to its build-time value when this package is
 * published. So the server decides and tells the browser, the same way it
 * already passes `window.__basename` and `window.__reactClientFetchTimeoutMs`
 * (see `wrapInHtmlDocument` in `ssr/page.renderer.ts`).
 *
 * In a browser the injected flag is the *only* signal consulted. Falling back
 * to the environment there would reopen the hole for any consumer whose
 * bundler shims `process.env` — a fabricated empty env reads exactly like a
 * developer's machine. The flag is emitted only when the server exposes, so
 * production HTML never carries it and an absent flag fails closed.
 *
 * With no `window` this is a server render of a client boundary, and the
 * environment answers.
 *
 * ## Why this does not call `shouldExposeErrorStack()`
 *
 * That is the canonical guard and this must stay equivalent to it, but it lives
 * in `@putnami/runtime`, whose root barrel re-exports `./config` — the
 * filesystem-backed config loader, `node:fs` and `node:module` included.
 * `expose-stack.ts` says so itself: it is kept dependency-free precisely so the
 * browser never pulls that in. Importing the barrel from `src/client/**` puts
 * it in the published browser graph, which is the boundary
 * `test/entrypoints.test.ts` exists to police. The duplication is four lines
 * and `test/client/expose-errors.test.ts` pins the two implementations to the
 * same answer across the whole environment matrix, so they cannot drift.
 */
export function shouldExposeClientErrors(): boolean {
  if (typeof window !== 'undefined') return window.__putnamiExposeErrors === true;
  const runtime = globalThis as { process?: { env?: unknown } };
  if (runtime.process?.env == null) return false;
  const nodeEnv = readRuntimeEnv('NODE_ENV');
  return nodeEnv !== 'production' && nodeEnv !== 'prod' && !readRuntimeEnv('K_SERVICE');
}
