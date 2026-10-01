import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import type { RouterState } from 'react-router';
import type { NavigationDetail } from '../../src/client/router/navigation-event';
import { PUTNAMI_NAVIGATION_EVENT } from '../../src/client/router/navigation-event';
import { createNavigationTracker, routeOf } from '../../src/client/router/navigation-tracking';

/** Hand-rolled DOM event target: this repo has no happy-dom. */
class FakeWindow {
  readonly dispatched: NavigationDetail[] = [];

  addEventListener(): void {}

  removeEventListener(): void {}

  dispatchEvent(event: Event): boolean {
    if (event.type === PUTNAMI_NAVIGATION_EVENT) {
      this.dispatched.push((event as CustomEvent<NavigationDetail>).detail);
    }
    return true;
  }
}

type BrowserGlobals = typeof globalThis & { window?: FakeWindow };

interface StateOptions {
  pathname: string;
  search?: string;
  hash?: string;
  paths?: (string | undefined)[];
  navigation?: 'idle' | 'loading' | 'submitting';
}

const state = (options: StateOptions): RouterState =>
  ({
    location: { pathname: options.pathname, search: options.search ?? '', hash: options.hash ?? '' },
    matches: (options.paths ?? ['/']).map((path) => ({ route: { path } })),
    navigation: { state: options.navigation ?? 'idle' },
  }) as unknown as RouterState;

function installWindow(): FakeWindow {
  const fake = new FakeWindow();
  (globalThis as BrowserGlobals).window = fake;
  return fake;
}

function clearWindow(): void {
  (globalThis as BrowserGlobals).window = undefined;
}

describe('routeOf', () => {
  it('reports __unknown__ when nothing matched', () => {
    expect(routeOf(state({ pathname: '/nope', paths: [] }))).toBe('__unknown__');
  });

  it('reports / for the root match', () => {
    expect(routeOf(state({ pathname: '/', paths: ['/'] }))).toBe('/');
  });

  it('converts a react-router param to the file-route form', () => {
    expect(routeOf(state({ pathname: '/tasks/7', paths: ['/', 'tasks', ':id'] }))).toBe('/tasks/[id]');
  });

  it('converts a splat to the file-route rest form', () => {
    expect(routeOf(state({ pathname: '/docs/a/b', paths: ['/', 'docs', '*'] }))).toBe('/docs/[...rest]');
  });

  it('skips pathless layout routes when joining segments', () => {
    expect(routeOf(state({ pathname: '/tasks/7', paths: ['/', undefined, 'tasks', ':id'] }))).toBe('/tasks/[id]');
  });
});

describe('navigation tracker', () => {
  beforeEach(() => {
    clearWindow();
  });

  afterEach(() => {
    clearWindow();
  });

  it('does not dispatch for the location it was primed with', () => {
    const fake = installWindow();
    const initial = state({ pathname: '/tasks', search: '?page=1' });
    const dispatchIfMoved = createNavigationTracker(initial.location);

    dispatchIfMoved(initial);

    expect(fake.dispatched).toEqual([]);
  });

  it('dispatches once per move and carries the previous pathname', () => {
    const fake = installWindow();
    const initial = state({ pathname: '/', paths: ['/'] });
    const dispatchIfMoved = createNavigationTracker(initial.location);

    const next = state({ pathname: '/tasks/7', hash: '#top', paths: ['/', 'tasks', ':id'] });
    dispatchIfMoved(next);
    dispatchIfMoved(next);

    expect(fake.dispatched).toEqual([
      { pathname: '/tasks/7', search: '', hash: '#top', route: '/tasks/[id]', previous: '/' },
    ]);
  });

  it('treats a query-string change as a move', () => {
    const fake = installWindow();
    const dispatchIfMoved = createNavigationTracker(state({ pathname: '/tasks' }).location);

    dispatchIfMoved(state({ pathname: '/tasks', search: '?page=2', paths: ['/', 'tasks'] }));

    expect(fake.dispatched.map((entry) => entry.search)).toEqual(['?page=2']);
  });

  it('ignores states that are still navigating', () => {
    const fake = installWindow();
    const dispatchIfMoved = createNavigationTracker(state({ pathname: '/' }).location);

    dispatchIfMoved(state({ pathname: '/tasks', navigation: 'loading', paths: ['/', 'tasks'] }));
    dispatchIfMoved(state({ pathname: '/tasks', navigation: 'submitting', paths: ['/', 'tasks'] }));

    expect(fake.dispatched).toEqual([]);
  });

  it('reports no previous pathname when it was never primed', () => {
    const fake = installWindow();
    const dispatchIfMoved = createNavigationTracker();

    dispatchIfMoved(state({ pathname: '/tasks', paths: ['/', 'tasks'] }));

    expect(fake.dispatched[0]?.previous).toBeUndefined();
    expect(fake.dispatched[0]?.route).toBe('/tasks');
  });
});
