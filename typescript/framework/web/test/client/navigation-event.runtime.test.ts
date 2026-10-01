import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import {
  dispatchNavigation,
  type NavigationDetail,
  onNavigation,
  PUTNAMI_NAVIGATION_EVENT,
} from '../../src/client/router/navigation-event';

/** Hand-rolled DOM event target: this repo has no happy-dom. */
class FakeWindow {
  readonly listeners = new Map<string, Set<(event: Event) => void>>();

  addEventListener(type: string, handler: (event: Event) => void): void {
    const set = this.listeners.get(type) ?? new Set();
    set.add(handler);
    this.listeners.set(type, set);
  }

  removeEventListener(type: string, handler: (event: Event) => void): void {
    this.listeners.get(type)?.delete(handler);
  }

  dispatchEvent(event: Event): boolean {
    for (const handler of this.listeners.get(event.type) ?? []) {
      handler(event);
    }
    return true;
  }
}

type BrowserGlobals = typeof globalThis & { window?: FakeWindow };

const detail = (pathname: string): NavigationDetail => ({
  pathname,
  search: '',
  hash: '',
  route: pathname,
  previous: undefined,
});

function installWindow(): FakeWindow {
  const fake = new FakeWindow();
  (globalThis as BrowserGlobals).window = fake;
  return fake;
}

function clearWindow(): void {
  (globalThis as BrowserGlobals).window = undefined;
}

describe('navigation event seam', () => {
  beforeEach(() => {
    clearWindow();
  });

  afterEach(() => {
    clearWindow();
  });

  it('delivers the detail to a subscriber and stops after unsubscribe', () => {
    const fake = installWindow();
    const seen: NavigationDetail[] = [];
    const unsubscribe = onNavigation((received) => seen.push(received));

    expect(fake.listeners.get(PUTNAMI_NAVIGATION_EVENT)?.size).toBe(1);
    dispatchNavigation(detail('/tasks'));
    expect(seen).toEqual([detail('/tasks')]);

    unsubscribe();
    expect(fake.listeners.get(PUTNAMI_NAVIGATION_EVENT)?.size).toBe(0);
    dispatchNavigation(detail('/other'));
    expect(seen).toEqual([detail('/tasks')]);
  });

  it('dispatches under the shared event name so a raw DOM listener also sees it', () => {
    const fake = installWindow();
    let received: NavigationDetail | undefined;
    fake.addEventListener(PUTNAMI_NAVIGATION_EVENT, (event) => {
      received = (event as CustomEvent<NavigationDetail>).detail;
    });

    dispatchNavigation(detail('/tasks/7'));

    expect(PUTNAMI_NAVIGATION_EVENT).toBe('putnami:navigation');
    expect(received?.pathname).toBe('/tasks/7');
  });

  it('is inert without a window: no subscription, no dispatch, no throw', () => {
    let called = false;
    const unsubscribe = onNavigation(() => {
      called = true;
    });

    expect(() => dispatchNavigation(detail('/tasks'))).not.toThrow();
    expect(() => unsubscribe()).not.toThrow();
    expect(called).toBe(false);
  });

  it('does not dispatch when the window cannot dispatch events', () => {
    (globalThis as BrowserGlobals).window = {} as FakeWindow;

    expect(() => dispatchNavigation(detail('/tasks'))).not.toThrow();
  });
});
