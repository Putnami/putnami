import { Window } from 'happy-dom';
import React from 'react';

// Stands in for a browser tab: renders a memory router in a DOM, navigates, and
// prints where the window scrolled and which documents it loaded, as one JSON
// line.
//
// Usage: bun browser.ts
//
// It runs in its own process: React keeps the context values of a server
// render on the shared context objects, so a router rendered in a process that
// already rendered on the server would read that state.

/** One scroll the page made: `top <y>` for window.scrollTo, `into <id>` for scrollIntoView. */
export type Scroll = string;

export interface RouterNavigationReport {
  /** The scrolls of each navigation step, keyed by step name. */
  scrolls: Record<string, Scroll[]>;
  /** The URLs the window loaded as a new document. */
  documentLoads: string[];
  /** Where the router is after each redirect step, keyed by step name. */
  locations: Record<string, string>;
  /** Every render error React reported. */
  errors: string[];
}

const TIMEOUT_MS = 2000;

const browser = new Window({ url: 'http://localhost/' });
const documentLoads: string[] = [];
let scrolls: Scroll[] = [];
let scrollY = 0;
Object.defineProperty(browser, 'scrollY', { get: () => scrollY, configurable: true });
browser.scrollTo = ((_x: number, y: number) => {
  scrolls.push(`top ${y}`);
}) as typeof browser.scrollTo;
browser.HTMLElement.prototype.scrollIntoView = function scrollIntoView(this: { id: string }) {
  scrolls.push(`into ${this.id}`);
};
browser.location.assign = (url: string | URL) => {
  documentLoads.push(String(url));
};
Object.assign(globalThis, {
  window: browser,
  document: browser.document,
  navigator: browser.navigator,
  sessionStorage: browser.sessionStorage,
});

const { createRoot } = await import('react-dom/client');
const { createMemoryRouter, RouterProvider } = await import('react-router');
const { restoreScrollAtRoot } = await import('../../../src/client/router/scroll-restoration');
const { setRouterRoutes } = await import('../../../src/client/router/router-routes');
const { routerRedirect } = await import('../../../src/client/form/follow-redirect');

const errors: string[] = [];
const record = (error: unknown) => errors.push(error instanceof Error ? error.message : String(error));

/** Wait until `done` holds, for at most the timeout. */
async function waitFor(done: () => boolean): Promise<void> {
  const deadline = Date.now() + TIMEOUT_MS;
  while (!done() && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 20));

/** A loader answer that redirects to `location`, as the server sends it. */
const serverRedirect = (location: string) =>
  new Response(null, { status: 204, headers: { 'X-Putnami-Redirect': location } });

const page = (name: string) => () =>
  React.createElement('main', null, React.createElement('h1', null, name), React.createElement('p', { id: 'target' }));

const routes = [
  {
    path: '/',
    id: 'root-layout',
    Component: () => React.createElement('div', { id: 'layout' }, React.createElement(Outlet)),
    children: [
      { path: 'a', id: 'root-a-page', Component: page('a') },
      { path: 'b', id: 'root-b-page', Component: page('b') },
      {
        path: 'to-login',
        id: 'root-to-login-page',
        loader: ({ request }: { request: Request }) =>
          routerRedirect(serverRedirect('/login'), request.url, () => false),
        Component: page('to-login'),
      },
      {
        path: 'to-b',
        id: 'root-to-b-page',
        loader: ({ request }: { request: Request }) => routerRedirect(serverRedirect('/b'), request.url, () => false),
        Component: page('to-b'),
      },
      { path: '*', Component: page('not found') },
    ],
  },
];
const { Outlet } = await import('react-router');

restoreScrollAtRoot(routes[0] as never);
setRouterRoutes(routes as never);
const router = createMemoryRouter(routes as never, { initialEntries: ['/a'] });
const container = browser.document.createElement('div');
browser.document.body.appendChild(container);
const root = createRoot(container as unknown as HTMLElement, {
  onUncaughtError: record,
  onCaughtError: record,
  onRecoverableError: record,
});
root.render(React.createElement(RouterProvider, { router }));
await waitFor(() => container.querySelector('h1') !== null);
await settle();

const report: RouterNavigationReport = { scrolls: {}, documentLoads, locations: {}, errors };

/**
 * Run one navigation step from scroll position `y` and keep the scrolls it
 * made. The scroll runs in a layout effect of the commit that renders the new
 * page, and React commits that page after the router turns idle, so the step
 * waits until the page shows `heading`.
 */
async function step(name: string, y: number, heading: string, go: () => Promise<void>): Promise<void> {
  scrollY = y;
  scrolls = [];
  await go();
  await waitFor(() => container.querySelector('h1')?.textContent === heading);
  report.scrolls[name] = scrolls;
}

const href = () => `${router.state.location.pathname}${router.state.location.search}${router.state.location.hash}`;

await step('newPage', 400, 'b', () => router.navigate('/b'));
await step('hash', 250, 'a', () => router.navigate('/a#target'));
await step('back', 0, 'b', () => router.navigate(-1));
await step('preventScrollReset', 120, 'a', () => router.navigate('/a', { preventScrollReset: true }));

await router.navigate('/to-login');
await settle();
report.locations['toLogin'] = href();
await router.navigate('/to-b');
await waitFor(() => router.state.navigation.state === 'idle');
report.locations['toB'] = href();

process.stdout.write(`${JSON.stringify(report)}\n`);
// A router keeps timers and listeners alive, so the process does not end alone.
process.exit(0);
