import { generatePath } from 'react-router';
import { LOADER_REQUEST_HEADER } from '../../shared/client-redirect';
import { clientFetchTimeoutMs } from './fetch-timeout';
import { routerRedirect } from './follow-redirect';

type LoaderHandlerArgs = {
  request: Request;
  params: Record<string, string>;
  // React Router's experimental matched-pattern field. Kept only as a
  // last-resort fallback — see `resolveLayoutPath`.
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
 * Resolve the path of a layout's JSON endpoint, without basename or suffix.
 *
 * The pattern is the framework-owned one threaded by the route generator
 * (`layoutLoaderHandler('/w/:workspace')`), so the derivation does not depend on
 * React Router's experimental `unstable_pattern` field, which can be renamed or
 * removed across react-router 7.x minors. `unstable_pattern` is used only when
 * the framework did not supply a pattern (e.g. an older generated bundle).
 *
 * A trailing catch-all is dropped so the endpoint matches the layout's mount
 * path (`/docs/*` -> `/docs`). Each dynamic segment is filled from the matched
 * params and URL-encoded, as React Router's `generatePath` does
 * (`/w/:workspace` with `{ workspace: 'a b' }` -> `/w/a%20b`); an optional
 * segment without a value is dropped. A required segment without a value
 * throws, so the route's error boundary renders instead of a request to a
 * literal `/:name` URL.
 */
function resolveLayoutPath(args: LoaderHandlerArgs, frameworkPattern?: string): string | undefined {
  const pattern = frameworkPattern ?? args.unstable_pattern;
  if (pattern === undefined) {
    return undefined;
  }
  return generatePath(pattern.replace(/\/\*$/, ''), args.params);
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

  // For layouts, build the URL from the route pattern filled with the matched
  // params (e.g., '/docs', '/w/acme') instead of the full request URL (e.g.,
  // '/docs/principles'). This ensures layout data is fetched from the correct
  // endpoint (/docs-layout.json, not /docs/principles-layout.json). The pattern
  // is supplied by the framework's own route metadata (see resolveLayoutPath);
  // the request URL already carries the mounted basename, but a path built from
  // the pattern does not, so it is re-applied.
  // Both endpoints keep the request's query string: the suffix goes on the path
  // (`/tasks.json?page=2`), so the loader reads the same query as on a full page
  // load.
  const requestUrl = new URL(args.request.url);
  const endpointSuffix = `${suffix ? `-${suffix}` : ''}.json`;
  let url: string;
  const layoutPath = usePattern ? resolveLayoutPath(args, layoutPattern) : undefined;
  if (layoutPath !== undefined) {
    url = `${requestUrl.origin}${clientBasename()}${layoutPath || '/'}${endpointSuffix}${requestUrl.search}`;
  } else {
    requestUrl.pathname += endpointSuffix;
    url = requestUrl.href;
  }

  const res = await fetch(url, {
    headers: { Accept: 'application/json', [LOADER_REQUEST_HEADER]: '1' },
    signal: AbortSignal.timeout(clientFetchTimeoutMs()),
  });

  // A loader that redirects: resolved against the page the router navigates
  // to, not the JSON endpoint, as the server render does on a full page load.
  // A followed redirect is data only when it ends on a loader endpoint: a page
  // answers the JSON request with an error, never with loader data.
  return routerRedirect(res, args.request.url, isLoaderEndpoint) ?? res;
}

/** Whether a followed loader fetch ended on a loader JSON endpoint. */
function isLoaderEndpoint(followed: Response): boolean {
  return new URL(followed.url).pathname.endsWith('.json');
}
