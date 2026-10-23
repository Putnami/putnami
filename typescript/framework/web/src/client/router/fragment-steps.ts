import type { DataRouter, RouteObject, ShouldRevalidateFunction, ShouldRevalidateFunctionArgs } from 'react-router';

/** Connects fragment-step handling to the browser router built after it. */
export interface FragmentSteps {
  /**
   * Starts reading the router's committed location.
   *
   * @param router - The browser router built on the routes `followFragmentSteps` received.
   */
  attach(router: DataRouter): void;
}

type RouterState = DataRouter['state'];

/** The parts of a URL a history step compares. */
interface CommittedUrl {
  pathAndSearch: string;
  hash: string;
}

const pathAndSearch = (location: { pathname: string; search: string }): string =>
  `${location.pathname}${location.search}`;

const committedUrl = (location: Location): CommittedUrl => ({
  pathAndSearch: pathAndSearch(location),
  hash: location.hash,
});

/** The 8-character random key React Router gives each history entry it creates. */
const createEntryKey = (): string => Math.random().toString(36).substring(2, 10);

const isSettled = (state: RouterState): boolean => state.navigation.state === 'idle' && state.revalidation === 'idle';

/**
 * Prepares a browser router for the history steps the browser makes on its
 * own: a link to an in-page `#id`, and back and forward between entries that
 * differ only by their fragment.
 *
 * - The entry the browser creates for a fragment link has a `null` state. It
 *   receives the state shape React Router writes, with a key of its own, so
 *   scroll restoration saves and restores its position apart from the entry
 *   it came from. Its `idx` stays `null`, as the router leaves it for an entry
 *   it did not create.
 * - A back or forward step that changes or drops only the fragment keeps the
 *   loaded data: every route's `shouldRevalidate` receives
 *   `defaultShouldRevalidate: false` for that step. A route that declares
 *   `shouldRevalidate` can still return `true`. A submission, a `revalidate()`
 *   call, a link, and a step taken while the router loads keep React Router's
 *   default.
 *
 * Call it before `createBrowserRouter`, and call `attach` after it: one
 * `popstate` listener runs before the router's, and one after it. The router
 * decides which loaders to run between the two, because `hydratePage` gives it
 * no `dataStrategy`, `getContext` or `patchRoutesOnNavigation`.
 *
 * @param win - The window the router reads its history from.
 * @param routes - The route tree the router is built on, changed in place.
 */
export function followFragmentSteps(win: Window, routes: RouteObject[]): FragmentSteps {
  let router: DataRouter | undefined;
  let committed = committedUrl(win.location);
  let fragmentStep = false;

  win.addEventListener(
    'popstate',
    () => {
      if (win.history.state === null) {
        win.history.replaceState({ usr: null, key: createEntryKey(), idx: null }, '');
      }
      fragmentStep =
        router !== undefined &&
        isSettled(router.state) &&
        committed.pathAndSearch === pathAndSearch(win.location) &&
        committed.hash !== win.location.hash;
    },
    { capture: true },
  );

  const keepsData = (args: ShouldRevalidateFunctionArgs): boolean =>
    fragmentStep && args.formMethod === undefined && pathAndSearch(args.currentUrl) === pathAndSearch(args.nextUrl);

  const keepDataOnFragmentSteps =
    (own: ShouldRevalidateFunction | null | undefined): ShouldRevalidateFunction =>
    (args) => {
      const decided = keepsData(args) ? { ...args, defaultShouldRevalidate: false } : args;
      const choice = own?.(decided);
      return typeof choice === 'boolean' ? choice : decided.defaultShouldRevalidate;
    };

  // React Router ignores the `shouldRevalidate` a lazy route loads when the
  // route also declares one, so the declared one is the one to wrap.
  const wrap = (route: RouteObject): void => {
    const { lazy } = route;
    if (route.shouldRevalidate || lazy === undefined) {
      route.shouldRevalidate = keepDataOnFragmentSteps(route.shouldRevalidate);
    } else if (typeof lazy === 'function') {
      route.lazy = async () => {
        const loaded = await lazy();
        return { ...loaded, shouldRevalidate: keepDataOnFragmentSteps(loaded.shouldRevalidate) };
      };
    } else if (lazy.shouldRevalidate) {
      const loadOwn = lazy.shouldRevalidate;
      route.lazy = { ...lazy, shouldRevalidate: async () => keepDataOnFragmentSteps(await loadOwn()) };
    } else {
      route.shouldRevalidate = keepDataOnFragmentSteps(undefined);
    }
    route.children?.forEach(wrap);
  };
  routes.forEach(wrap);

  return {
    attach(created) {
      router = created;
      committed = committedUrl(win.location);
      win.addEventListener('popstate', () => {
        fragmentStep = false;
      });
      created.subscribe((state) => {
        if (state.navigation.state === 'idle') {
          committed = committedUrl(win.location);
        }
      });
    },
  };
}
