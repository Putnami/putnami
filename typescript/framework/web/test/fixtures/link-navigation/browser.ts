import { Window } from 'happy-dom';
import React from 'react';

// Stands in for a browser tab: renders Links inside a real memory router in a
// DOM, clicks them, and prints what the router did as one JSON line.
//
// Usage: bun browser.ts
//
// It runs in its own process: React keeps the context values of a server
// render on the shared context objects, so a router rendered in a process that
// already rendered on the server would read that state.

export interface LinkClickResult {
  /** The location the router reached: pathname, search and hash. */
  href: string;
  /** How the router reached it: `PUSH` or `REPLACE`. */
  historyAction: string;
  /** How many times the route loader ran for the click. */
  loads: number;
}

export interface LinkNavigationReport {
  /** Each scenario's result, keyed by scenario name. */
  results: Record<string, LinkClickResult>;
  /** Every render error React reported. */
  errors: string[];
}

const TIMEOUT_MS = 2000;

const browser = new Window({ url: 'http://localhost/' });
Object.assign(globalThis, {
  window: browser,
  document: browser.document,
  navigator: browser.navigator,
  MouseEvent: browser.MouseEvent,
});

const { createRoot } = await import('react-dom/client');
const { createMemoryRouter, RouterProvider } = await import('react-router');
const { Link } = await import('../../../src/client/components');

let loads = 0;
const errors: string[] = [];

const Page = () =>
  React.createElement(
    'nav',
    null,
    React.createElement(Link, { 'data-testid': 'query', to: { pathname: '/tasks', search: '?env=qa' } }, 'QA'),
    React.createElement(Link, { 'data-testid': 'clear', to: '/tasks' }, 'All'),
    React.createElement(
      Link,
      { 'data-testid': 'hash', to: { pathname: '/tasks', search: '?page=2', hash: '#details' } },
      'Details',
    ),
  );

const router = createMemoryRouter(
  [
    {
      path: '/tasks',
      loader: () => {
        loads += 1;
        return null;
      },
      element: React.createElement(Page),
    },
  ],
  { initialEntries: ['/tasks?page=2'] },
);

const container = browser.document.createElement('div');
browser.document.body.appendChild(container);
const record = (error: unknown) => errors.push(error instanceof Error ? error.message : String(error));
const root = createRoot(container as unknown as HTMLElement, {
  onUncaughtError: record,
  onCaughtError: record,
  onRecoverableError: record,
});
root.render(React.createElement(RouterProvider, { router }));

/** Wait until `done` holds, for at most the timeout. */
async function waitFor(done: () => boolean): Promise<void> {
  const deadline = Date.now() + TIMEOUT_MS;
  while (!done() && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

const idle = () => router.state.navigation.state === 'idle';

/** Start at `from`, click the link `testId`, and report what the router did. */
async function scenario(from: string, testId: string): Promise<LinkClickResult> {
  await router.navigate(from);
  await waitFor(idle);
  const selector = `[data-testid="${testId}"]`;
  await waitFor(() => container.querySelector(selector) !== null);
  loads = 0;
  const startKey = router.state.location.key;
  container
    .querySelector(selector)
    ?.dispatchEvent(new browser.MouseEvent('click', { bubbles: true, cancelable: true, button: 0 }));
  await waitFor(() => router.state.location.key !== startKey && idle());
  const { pathname, search, hash } = router.state.location;
  return { href: `${pathname}${search}${hash}`, historyAction: router.state.historyAction, loads };
}

const report: LinkNavigationReport = {
  results: {
    queryOnly: await scenario('/tasks?page=2', 'query'),
    queryRemoved: await scenario('/tasks?page=2', 'clear'),
    sameUrl: await scenario('/tasks', 'clear'),
    hashOnly: await scenario('/tasks?page=2', 'hash'),
  },
  errors,
};
process.stdout.write(`${JSON.stringify(report)}\n`);
// A router keeps timers and listeners alive, so the process does not end alone.
process.exit(0);
