import { describe, expect, it } from 'bun:test';
import type { DataRouter, RouteObject, ShouldRevalidateFunction, ShouldRevalidateFunctionArgs } from 'react-router';
import { followFragmentSteps } from '../../src/client/router/fragment-steps';

type Listener = () => void;
type FakeState = { navigation: { state: string }; revalidation: string };

const IDLE: FakeState = { navigation: { state: 'idle' }, revalidation: 'idle' };

/** The parts of a window `followFragmentSteps` reads, with a history whose entries the test sets. */
function fakeWindow(href: string) {
  const base = new URL(href);
  const listeners: Listener[] = [];
  const at = (url: URL) => ({ pathname: url.pathname, search: url.search, hash: url.hash });
  const win = {
    location: at(base),
    history: {
      state: { usr: null, key: 'default', idx: 0 } as unknown,
      replaceState(state: unknown) {
        this.state = state;
      },
    },
    addEventListener: (type: string, listener: Listener) => {
      if (type === 'popstate') {
        listeners.push(listener);
      }
    },
  };
  /**
   * A history step to `next`, whose entry carries `state`. `routerTurn` runs
   * where the router's own `popstate` listener runs: after the listener
   * `followFragmentSteps` adds and before the one `attach` adds.
   */
  const pop = (next: string, state: unknown, routerTurn: () => void = () => undefined) => {
    win.location = at(new URL(next, base));
    win.history.state = state;
    const [before, ...after] = listeners;
    before?.();
    routerTurn();
    for (const listener of after) {
      listener();
    }
  };
  return { win, pop };
}

/** A router whose state the test sets and whose subscribers it notifies. */
function fakeRouter() {
  const subscribers: Array<(state: FakeState) => void> = [];
  const router = {
    state: IDLE,
    subscribe: (subscriber: (state: FakeState) => void) => {
      subscribers.push(subscriber);
      return () => undefined;
    },
  };
  const update = (state: FakeState) => {
    router.state = state;
    for (const subscriber of subscribers) {
      subscriber(state);
    }
  };
  return { router, update };
}

/** The arguments React Router passes a route's `shouldRevalidate` for a step from `from` to `to`. */
const args = (from: string, to: string, extra: Partial<ShouldRevalidateFunctionArgs> = {}) =>
  ({
    currentUrl: new URL(from, 'http://localhost'),
    nextUrl: new URL(to, 'http://localhost'),
    currentParams: {},
    nextParams: {},
    defaultShouldRevalidate: true,
    ...extra,
  }) as ShouldRevalidateFunctionArgs;

/** Builds the routes and the window, and attaches a router, as `hydratePage` does. */
function setup(routes: RouteObject[], href = 'http://localhost/page#section') {
  const { win, pop } = fakeWindow(href);
  const steps = followFragmentSteps(win as unknown as Window, routes);
  const fake = fakeRouter();
  steps.attach(fake.router as unknown as DataRouter);
  return { win, pop, ...fake };
}

const decide = (route: RouteObject, input: ShouldRevalidateFunctionArgs) =>
  (route.shouldRevalidate as ShouldRevalidateFunction)(input);

/** What `route` decides for a back step from `/page#section` to `next`, taken from the router's turn. */
function decideOnPop(
  pop: ReturnType<typeof setup>['pop'],
  route: RouteObject,
  next: string,
  extra: Partial<ShouldRevalidateFunctionArgs> = {},
) {
  let decision: boolean | undefined;
  pop(next, { idx: 0 }, () => {
    decision = decide(route, args('/page#section', next, extra));
  });
  return decision;
}

describe('followFragmentSteps', () => {
  it('gives the entry of a fragment link a key of its own', () => {
    const { win, pop } = setup([{ path: '/' }], 'http://localhost/page');

    pop('/page#section', null);

    expect(win.history.state).toMatchObject({ usr: null, idx: null });
    expect((win.history.state as { key: string }).key).toMatch(/^[a-z0-9]{1,8}$/);
  });

  it('keeps the state of an entry the router created', () => {
    const { win, pop } = setup([{ path: '/' }]);
    const state = { usr: null, key: 'abc12345', idx: 1 };

    pop('/page', state);

    expect(win.history.state).toBe(state);
  });

  it('keeps the loaded data on a step that drops only the fragment', () => {
    const child: RouteObject = { path: 'page', loader: () => null };
    const route: RouteObject = { path: '/', loader: () => null, children: [child] };
    const { pop } = setup([route]);

    let decisions: boolean[] = [];
    pop('/page', { idx: 0 }, () => {
      decisions = [decide(route, args('/page#section', '/page')), decide(child, args('/page#section', '/page'))];
    });

    expect(decisions).toEqual([false, false]);
  });

  it('lets a route that declares shouldRevalidate run its loader on that step', () => {
    const seen: boolean[] = [];
    const route: RouteObject = {
      path: '/',
      shouldRevalidate: ({ defaultShouldRevalidate }) => {
        seen.push(defaultShouldRevalidate);
        return true;
      },
    };
    const { pop } = setup([route]);

    expect(decideOnPop(pop, route, '/page')).toBe(true);
    expect(seen).toEqual([false]);
  });

  it("keeps React Router's default for a step that changes the path or the query", () => {
    const route: RouteObject = { path: '/' };
    const { pop } = setup([route]);

    expect(decideOnPop(pop, route, '/other')).toBe(true);
    expect(decideOnPop(pop, route, '/page?tab=2')).toBe(true);
  });

  it("keeps React Router's default for a step between entries with the same fragment", () => {
    const route: RouteObject = { path: '/' };
    const { pop } = setup([route], 'http://localhost/page');

    let decision: boolean | undefined;
    pop('/page', { idx: 0 }, () => {
      decision = decide(route, args('/page', '/page'));
    });

    expect(decision).toBe(true);
  });

  it("keeps React Router's default for a submission", () => {
    const route: RouteObject = { path: '/' };
    const { pop } = setup([route]);

    expect(decideOnPop(pop, route, '/page', { formMethod: 'POST' })).toBe(true);
  });

  it("keeps React Router's default for a step taken while the router loads", () => {
    const route: RouteObject = { path: '/' };
    const { pop, router } = setup([route]);
    router.state = { navigation: { state: 'loading' }, revalidation: 'idle' };

    expect(decideOnPop(pop, route, '/page')).toBe(true);
  });

  it("keeps React Router's default after the step, for a link or a revalidation", () => {
    const route: RouteObject = { path: '/' };
    const { pop } = setup([route]);

    pop('/page', { idx: 0 });

    expect(decide(route, args('/page#section', '/page'))).toBe(true);
  });

  it('compares the next step with the location the router committed', () => {
    const route: RouteObject = { path: '/' };
    const { pop, win, update } = setup([route]);

    pop('/page', { idx: 0 });
    win.location = { pathname: '/other', search: '', hash: '' };
    update(IDLE);

    expect(decideOnPop(pop, route, '/other#section')).toBe(true);
    let decision: boolean | undefined;
    pop('/other#top', { idx: 1 }, () => {
      decision = decide(route, args('/other#section', '/other#top'));
    });
    expect(decision).toBe(false);
  });

  it('applies to the shouldRevalidate a lazy route loads', async () => {
    const lazyFunction: RouteObject = {
      path: '/',
      lazy: async () => ({ shouldRevalidate: ({ defaultShouldRevalidate }) => defaultShouldRevalidate }),
    };
    const lazyObject: RouteObject = {
      path: '/b',
      lazy: {
        shouldRevalidate:
          async () =>
          ({ defaultShouldRevalidate }) =>
            defaultShouldRevalidate,
      },
    };
    const { pop } = setup([lazyFunction, lazyObject]);

    const loaded = await (lazyFunction.lazy as () => Promise<RouteObject>)();
    const loadShouldRevalidate = (lazyObject.lazy as { shouldRevalidate: () => Promise<ShouldRevalidateFunction> })
      .shouldRevalidate;
    const fromObject = await loadShouldRevalidate();

    let decisions: boolean[] = [];
    pop('/page', { idx: 0 }, () => {
      decisions = [decide(loaded, args('/page#section', '/page')), fromObject(args('/page#section', '/page'))];
    });
    expect(decisions).toEqual([false, false]);
  });

  it('wraps a declared shouldRevalidate and leaves the lazy route as it is', () => {
    const lazy = async () => ({});
    const route: RouteObject = { path: '/', lazy, shouldRevalidate: () => undefined as unknown as boolean };
    const { pop } = setup([route]);

    expect(route.lazy).toBe(lazy);
    expect(decideOnPop(pop, route, '/page')).toBe(false);
  });
});
