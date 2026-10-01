/**
 * Reads an environment variable at runtime, through an indirection bundlers
 * cannot constant-fold.
 *
 * `bun build` (like esbuild) substitutes the literal member expression
 * `process.env.NODE_ENV` with its build-time value on **every** target —
 * `browser`, `bun` and `node` alike. A guard written that way is therefore
 * frozen when this package is published: the consumer's own `NODE_ENV` can
 * never change it, and `process.env.NODE_ENV !== 'production'` ships as a
 * literal `true`. Going through `globalThis` and indexing `env` with a
 * variable defeats that substitution, so the decision is taken in the
 * consumer's process. Optional chaining keeps the read safe in a browser
 * bundle, where `process` does not exist.
 *
 * Not exported from the package: the fold-resistant shape is an implementation
 * detail of the guards below, not a general configuration API — use
 * `useConfig()` for that.
 */
function readRuntimeEnv(name: string): string | undefined {
  const runtime = globalThis as { process?: { env?: Record<string, string | undefined> } };
  return runtime.process?.env?.[name];
}

/** Whether this runtime carries an environment at all. A browser does not. */
function hasProcessEnv(): boolean {
  const runtime = globalThis as { process?: { env?: unknown } };
  return runtime.process?.env != null;
}

/**
 * Whether serialized HTTP error responses should include the stack trace.
 *
 * Stacks are exposed in every non-production environment and hidden in production.
 * Keep this helper dependency-free: `HttpException` is used in browser bundles,
 * so importing the config loader here would pull filesystem-backed config sources
 * into client builds.
 *
 * A runtime with no `process.env` — a browser bundle — reports `false`. It has
 * no environment to read, so it cannot assert that it is a development
 * environment, and a guard that cannot tell must assume production. Note this
 * is deliberately narrower than "NODE_ENV is unset": a server process with no
 * `NODE_ENV` still exposes, because running `bun run` without setting it is a
 * normal development shape.
 *
 * `@putnami/web` re-opens the browser case in development through
 * `shouldExposeClientErrors()`, which reads a flag SSR injects into the page.
 * That keeps the server-rendered and hydrated markup agreeing in *both*
 * environments; deciding it here would only ever agree in one.
 */
export function shouldExposeErrorStack(): boolean {
  if (!hasProcessEnv()) return false;
  const nodeEnv = readRuntimeEnv('NODE_ENV');
  return nodeEnv !== 'production' && nodeEnv !== 'prod' && !readRuntimeEnv('K_SERVICE');
}
