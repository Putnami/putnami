import { Window } from 'happy-dom';
import React from 'react';

// Stands in for a browser tab: hydrates a page with `hydratePage`, follows a
// link to an in-page `#id` the way a browser does, goes back and forward, and
// prints where the window scrolled and which loaders ran, as one JSON line.
//
// Usage: bun browser.ts
//
// It runs in its own process: React keeps the context values of a server
// render on the shared context objects, so a router rendered in a process that
// already rendered on the server would read that state.

/** One scroll the page made: `top <y>` for window.scrollTo, `into <id>` for scrollIntoView. */
export type Scroll = string;

export interface FragmentStepResult {
  /** The location the page rendered after the step: pathname, search and hash. */
  rendered: string;
  /** The scrolls the step made. */
  scrolls: Scroll[];
  /** The loaders the step ran, by route id. */
  loads: string[];
}

export interface FragmentNavigationReport {
  /** Each step's result, keyed by step name. */
  steps: Record<string, FragmentStepResult>;
  /** Every error React or `hydratePage` reported. */
  errors: string[];
}

const TIMEOUT_MS = 2000;
/** Where the fragment target sits below the top of the page. */
const SECTION_Y = 1689;

const errors: string[] = [];
console.error = (...args: unknown[]) => {
  errors.push(args.map((arg) => (arg instanceof Error ? arg.message : String(arg))).join(' '));
};

const browser = new Window({ url: 'http://localhost/page' });
let scrolls: Scroll[] = [];
let scrollY = 0;
Object.defineProperty(browser, 'scrollY', { get: () => scrollY, configurable: true });
browser.scrollTo = ((_x: number, y: number) => {
  scrolls.push(`top ${y}`);
}) as typeof browser.scrollTo;
browser.HTMLElement.prototype.scrollIntoView = function scrollIntoView(this: { id: string }) {
  scrolls.push(`into ${this.id}`);
};
Object.assign(globalThis, {
  window: browser,
  document: browser.document,
  navigator: browser.navigator,
  sessionStorage: browser.sessionStorage,
  MouseEvent: browser.MouseEvent,
  requestAnimationFrame: browser.requestAnimationFrame.bind(browser),
});

const { createPath, Link, Outlet, useLocation, useRevalidator } = await import('react-router');
const { hydratePage } = await import('../../../src/client/router/hydrate');

let loads: string[] = [];
const loader = (id: string) => () => {
  loads.push(id);
  return { id };
};

const Layout = () => React.createElement('div', { id: 'layout' }, React.createElement(Outlet));
function Page() {
  const revalidator = useRevalidator();
  return React.createElement(
    'main',
    null,
    React.createElement('output', { 'data-testid': 'rendered' }, createPath(useLocation())),
    React.createElement('a', { 'data-testid': 'fragment', href: '#section' }, 'Section'),
    React.createElement(Link, { 'data-testid': 'page', to: '/page' }, 'Page'),
    React.createElement(Link, { 'data-testid': 'query', to: '?tab=2' }, 'Tab'),
    React.createElement(
      'button',
      { 'data-testid': 'revalidate', type: 'button', onClick: () => revalidator.revalidate() },
      'Reload',
    ),
    React.createElement('p', { id: 'section' }, 'Section'),
  );
}

const routes = [
  {
    path: '/',
    id: 'layout',
    loader: loader('layout'),
    Component: Layout,
    children: [{ path: 'page', id: 'page', loader: loader('page'), Component: Page }],
  },
];

// The markup the server sends for `/page`, and the loader data it serializes.
const document = browser.document;
document.write(
  '<!doctype html><html><body><div id="root"><div id="layout"><main>' +
    '<output data-testid="rendered">/page</output>' +
    '<a data-testid="fragment" href="#section">Section</a>' +
    '<a data-testid="page" href="/page" data-discover="true">Page</a>' +
    '<a data-testid="query" href="/page?tab=2" data-discover="true">Tab</a>' +
    '<button data-testid="revalidate" type="button">Reload</button>' +
    '<p id="section">Section</p>' +
    '</main></div></div></body></html>',
);
Object.assign(browser, {
  __staticRouterHydrationData: { loaderData: { layout: { id: 'layout' }, page: { id: 'page' } } },
});

/** Wait until `done` holds, for at most the timeout. */
async function waitFor(done: () => boolean): Promise<void> {
  const deadline = Date.now() + TIMEOUT_MS;
  while (!done() && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 50));
const rendered = () => document.querySelector('[data-testid="rendered"]')?.textContent ?? '';

await hydratePage('root', routes as never);
await settle();

const report: FragmentNavigationReport = { steps: {}, errors };

/**
 * Run one step from scroll position `y`, wait until the page renders `target`
 * and settles, and keep the scrolls and loads it made.
 */
async function step(name: string, y: number, target: string, go: () => void): Promise<void> {
  scrollY = y;
  scrolls = [];
  loads = [];
  go();
  await waitFor(() => rendered() === target);
  await settle();
  report.steps[name] = { rendered: rendered(), scrolls, loads };
}

const click = (testId: string) =>
  document
    .querySelector(`[data-testid="${testId}"]`)
    ?.dispatchEvent(new browser.MouseEvent('click', { bubbles: true, cancelable: true, button: 0 }));

// A browser follows a link to `#section` as a fragment navigation: it adds a
// history entry with a null state, fires `popstate`, then scrolls to the
// target. The DOM adds the entry on the click and fires no `popstate`, so the
// step fires it and scrolls as the browser does.
await step('fragmentLink', 0, '/page#section', () => {
  click('fragment');
  browser.dispatchEvent(new browser.PopStateEvent('popstate', { state: browser.history.state }));
  scrollY = SECTION_Y;
});
await step('backDropsTheFragment', SECTION_Y, '/page', () => browser.history.back());
await step('forwardToTheFragment', 0, '/page#section', () => browser.history.forward());
// A link and a revalidation that drop or keep the fragment still run the loaders.
await step('linkDropsTheFragment', SECTION_Y, '/page', () => click('page'));
await step('revalidate', 0, '/page', () => click('revalidate'));
await step('queryLink', 0, '/page?tab=2', () => click('query'));

process.stdout.write(`${JSON.stringify(report)}\n`);
// A router keeps listeners on the window, so the process does not end alone.
process.exit(0);
