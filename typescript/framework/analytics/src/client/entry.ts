/**
 * Browser tracker entry point.
 *
 * The pre-build hook bundles this module into
 * `.gen/<publicFolder>/analytics/analytics.<hash>.js.gz` and the page-view
 * middleware injects the tag that loads it. It is a side-effect module: it
 * reads the bootstrap the server published and installs the tracker.
 *
 * There are two ways the bootstrap arrives, and the difference is what
 * rendered the page. A server-rendered page carries it in the hydration
 * script, as `window.__putnamiBootstrap.analytics`. A pre-rendered `.static()`
 * page has no hydration script — it was rendered at build time, with no
 * request — so the middleware puts the same object on the script tag it
 * injects into the response, where no CSP nonce is needed to read it.
 *
 * Without either, this installs nothing: a tracker with no server event id to
 * complete would emit a second page view for the same load.
 *
 * Nothing reachable from this file may import `@putnami/runtime`,
 * `@putnami/database`, `@putnami/application`, a `node:` module, or a file
 * under `src/server/**`. `test/entrypoints.test.ts` is the enforcement.
 */
import { installTracker } from './tracker';
import type { ClientBootstrap } from './wire';

/**
 * The attribute the injected tag carries the bootstrap in.
 *
 * The literal is repeated rather than imported: the server twin lives under
 * `src/server/**`, which nothing in the bundle may reach.
 * `test/entrypoints.test.ts` pins the two copies together.
 */
export const BOOTSTRAP_ATTRIBUTE = 'data-putnami-analytics';

/**
 * Resolves the bootstrap, hydration script first, injected tag second.
 *
 * Outside a browser it resolves to nothing, so importing this module in a test
 * runner installs no tracker and leaves no listener behind.
 *
 * @returns The bootstrap the server published, or `undefined`.
 */
export function readBootstrap(): unknown {
  const target = (globalThis as { window?: { __putnamiBootstrap?: { analytics?: unknown } } }).window;
  if (!target) {
    return undefined;
  }
  const published = target.__putnamiBootstrap?.analytics;
  if (published) {
    return published;
  }
  const raw = document.querySelector(`script[${BOOTSTRAP_ATTRIBUTE}]`)?.getAttribute(BOOTSTRAP_ATTRIBUTE);
  if (!raw) {
    return undefined;
  }
  try {
    return JSON.parse(raw);
  } catch {
    // A truncated or rewritten attribute is a page problem, not a reason to
    // throw inside someone else's document.
    return undefined;
  }
}

const boot = readBootstrap();

if (boot && typeof boot === 'object') {
  installTracker(boot as ClientBootstrap);
}
