import { clientFetchTimeoutMs } from './fetch-timeout';

type LoaderHandlerArgs = {
  request: Request;
  params: Record<string, string>;
  // React Router's experimental matched-pattern field. Kept only as a
  // last-resort fallback — see `resolveLayoutPattern`.
  unstable_pattern?: string;
  context: unknown;
};

/**
 * Read the mounted basename injected by SSR (`window.__basename`, e.g. `/docs`).
 * Absent during pure client-side navigation before hydration or when the app is
 * mounted at the root, in which case there is no prefix to apply.
 */
function clientBasename(): string {
  const basename = typeof window !== 'undefined' ? window.__basename : undefined;
  if (!basename || basename === '/') {
    return '';
  }
  // Normalize to a leading-slash, no-trailing-slash prefix so it composes
  // cleanly with an absolute route pattern (`/docs` + `/guide`).
  const withLeadingSlash = basename.startsWith('/') ? basename : `/${basename}`;
  return withLeadingSlash.endsWith('/') ? withLeadingSlash.slice(0, -1) : withLeadingSlash;
}

/**
 * Resolve the route pattern used to build a layout's JSON endpoint URL.
 *
 * Prefers the framework-owned pattern threaded by the route generator
 * (`layoutLoaderHandler('/docs')`) so the derivation does not depend on React
 * Router's experimental `unstable_pattern` field, which can be renamed or
 * removed across react-router 7.x minors. Falls back to `unstable_pattern` only
 * when the framework did not supply a pattern (e.g. an older generated bundle).
 */
function resolveLayoutPattern(args: LoaderHandlerArgs, frameworkPattern?: string): string | undefined {
  const pattern = frameworkPattern ?? args.unstable_pattern;
  if (pattern === undefined) {
    return undefined;
  }
  // Strip the trailing /* of catch-all routes so the layout endpoint matches
  // the layout's mount path (e.g. '/docs/*' -> '/docs').
  return pattern.replace(/\/\*$/, '');
}

export const pageLoaderHandler = () => async (args: LoaderHandlerArgs) => loaderHandler({ args });
export const layoutLoaderHandler = (pattern?: string) => async (args: LoaderHandlerArgs) =>
  loaderHandler({ args, suffix: 'layout', layoutPattern: pattern, usePattern: true });

async function loaderHandler(opt: {
  args: LoaderHandlerArgs;
  suffix?: string;
  usePattern?: boolean;
  layoutPattern?: string;
}) {
  const { args, suffix, usePattern, layoutPattern } = opt;

  // For layouts, build the URL from the route pattern (e.g., '/docs') instead of
  // the full request URL (e.g., '/docs/principles'). This ensures layout data is
  // fetched from the correct endpoint (/docs-layout.json, not
  // /docs/principles-layout.json). The pattern is supplied by the framework's own
  // route metadata (see resolveLayoutPattern); the request URL already carries the
  // mounted basename, but a reconstructed pattern does not, so it is re-applied.
  let baseUrl: string;
  const pattern = usePattern ? resolveLayoutPattern(args, layoutPattern) : undefined;
  if (pattern !== undefined) {
    const requestUrl = new URL(args.request.url);
    baseUrl = `${requestUrl.origin}${clientBasename()}${pattern || '/'}`;
  } else {
    baseUrl = args.request.url;
  }

  const url = `${baseUrl}${suffix ? `-${suffix}` : ''}.json`;

  return await fetch(url, {
    headers: { Accept: 'application/json' },
    signal: AbortSignal.timeout(clientFetchTimeoutMs()),
  });
}
