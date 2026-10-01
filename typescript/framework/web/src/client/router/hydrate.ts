import React from 'react';
import { hydrateRoot } from 'react-dom/client';
import { createBrowserRouter, matchRoutes, type RouteObject, RouterProvider, type RouterState } from 'react-router';
import { ANONYMOUS_SECURITY_CONTEXT, type ClientSecurityContext } from '../../shared/security.types';
import { SecurityContextProvider, setSecurityContext } from '../security/security-context';
import { createNavigationTracker } from './navigation-tracking';

/**
 * Type definition for the hydration data injected by the server.
 * This data is used to hydrate the React Router state on the client.
 * Matches the expected shape of React Router's hydration data.
 */
type StaticRouterHydrationData = Partial<Pick<RouterState, 'loaderData' | 'actionData' | 'errors'>>;

declare global {
  interface Window {
    __staticRouterHydrationData?: StaticRouterHydrationData;
    __basename?: string;
    __securityContext?: ClientSecurityContext;
  }
}

const HYDRATION_LOG_PREFIX = '[putnami:hydration]';

function reportHydrationError(
  kind: 'uncaught' | 'caught' | 'recoverable' | 'blanked',
  error: unknown,
  errorInfo: React.ErrorInfo,
): void {
  const message = error instanceof Error ? error.message : String(error);
  const stack = error instanceof Error ? error.stack : undefined;
  // biome-ignore lint/suspicious/noConsole: client-side error reporting requires console
  console.error(HYDRATION_LOG_PREFIX, kind, {
    message,
    stack,
    componentStack: errorInfo.componentStack,
    route: window.location.pathname,
  });
}

/**
 * Schedule a post-commit DOM check. `hydrateRoot` renders asynchronously, so a
 * container that React tears down and re-renders empty is NOT observable
 * synchronously after the call returns. A double `requestAnimationFrame` runs
 * after React has committed and the browser has painted at least once; fall back
 * to a macrotask when rAF is unavailable, and skip entirely in non-browser
 * contexts where neither exists (there is no live DOM to heal there).
 */
function scheduleServerContentGuard(check: () => void): void {
  if (typeof window !== 'undefined' && typeof window.requestAnimationFrame === 'function') {
    window.requestAnimationFrame(() => window.requestAnimationFrame(check));
    return;
  }
  if (typeof setTimeout === 'function') {
    setTimeout(check, 0);
  }
}

export async function hydratePage(containerId: string, routes: RouteObject[]) {
  const basename = window.__basename;

  // Read security context injected by SSR (roles, scopes, authenticated status).
  // Falls back to anonymous if no context was serialized.
  const securityContext: ClientSecurityContext = window.__securityContext ?? ANONYMOUS_SECURITY_CONTEXT;
  setSecurityContext(securityContext);

  const rootElem = document.getElementById(containerId);
  if (!rootElem) {
    throw new Error(`Hydration failed: root element with id "${containerId}" not found`);
  }

  // Snapshot the server-rendered markup up front. The SSR page is fully correct
  // on its own; if hydration blows up (a client chunk that throws at module
  // load, a broken component, a mismatch React can't recover) we restore this so
  // the user keeps the static server content instead of a torn-down blank page.
  const serverHtml = rootElem.innerHTML;
  const keepServerHtml = () => {
    // React may have already emptied the container while unwinding a failed
    // hydration; put the server HTML back so the page degrades to static
    // (non-interactive) content rather than a total outage.
    try {
      rootElem.innerHTML = serverHtml;
    } catch {
      // If we cannot restore the DOM there is nothing more to do; the error has
      // already been reported.
    }
  };

  // Preload matched lazy routes so they are available during initial hydration;
  // this prevents React from rendering a fallback and mismatching the server
  // HTML. A chunk that fails to load here (the exact "throws at module load"
  // failure mode) must NOT reject out of hydratePage as an unhandled rejection —
  // that would leave the page in an indeterminate state. Instead we report it
  // and skip hydration, keeping the static server content.
  let preloadFailed = false;
  const matches = matchRoutes(routes, window.location.pathname, basename);
  if (matches) {
    await Promise.all(
      matches.map(async (m) => {
        const route = m.route;
        if (route.lazy) {
          const lazyFn = route.lazy as () => Promise<Partial<RouteObject>>;
          try {
            const result = await lazyFn();
            Object.assign(route, { ...result, lazy: undefined });
          } catch (error) {
            preloadFailed = true;
            reportHydrationError('uncaught', error, { componentStack: null });
          }
        }
      }),
    );
  }

  if (preloadFailed) {
    // The active route's client chunk could not be loaded — hydrating now would
    // tear down the SSR tree. Leave the server HTML in place as static content.
    return undefined;
  }

  const router = createBrowserRouter(routes, {
    basename,
    hydrationData: window.__staticRouterHydrationData,
  });

  // Announce client-side route changes on the DOM so plugins can observe
  // navigation without patching the router. Priming the tracker with the
  // current location means hydration itself dispatches nothing: the server
  // already rendered — and already recorded — this view.
  router.subscribe(createNavigationTracker(router.state.location));

  const routerElement = React.createElement(RouterProvider, { router });

  const elem = React.createElement(SecurityContextProvider, { value: securityContext }, routerElement);

  // Container-level safety net that does NOT depend on which internal React path
  // fired. React 19's hydration-recovery path can discard the SSR subtree and
  // commit an empty outlet WITHOUT invoking any of the callbacks below — leaving
  // a blank page with a clean console (the exact failure this guards against).
  // After React has had a chance to commit, if the outlet ended up empty while
  // the server had rendered content, restore the SSR HTML (degrade to static
  // content) and report it so a blanking is never silent.
  const verifyServerContentPreserved = () => {
    const blanked = rootElem.innerHTML.trim() === '';
    const hadServerHtml = serverHtml.trim() !== '';
    if (blanked && hadServerHtml) {
      keepServerHtml();
      reportHydrationError('blanked', new Error('Hydration left the outlet empty; restored server-rendered HTML'), {
        componentStack: null,
      });
    }
  };

  try {
    const root = hydrateRoot(rootElem, elem, {
      onUncaughtError: (error, errorInfo) => {
        reportHydrationError('uncaught', error, errorInfo);
        keepServerHtml();
      },
      onCaughtError: (error, errorInfo) => {
        reportHydrationError('caught', error, errorInfo);
      },
      onRecoverableError: (error, errorInfo) => {
        reportHydrationError('recoverable', error, errorInfo);
      },
    });
    scheduleServerContentGuard(verifyServerContentPreserved);
    return root;
  } catch (error) {
    // hydrateRoot threw synchronously (e.g. an invalid container); keep the
    // server HTML instead of surfacing a blank page.
    reportHydrationError('uncaught', error, { componentStack: null });
    keepServerHtml();
    return undefined;
  }
}
