import type { ClientBootstrap } from '../../src/client/wire';

/**
 * Hand-rolled browser globals for the tracker tests.
 *
 * There is no `happy-dom` in this repository, and the tracker is written to
 * read `window`, `document`, `navigator`, and storage *inside* functions
 * precisely so a fake installed in `beforeEach` is the one it sees. The style
 * is the one `typescript/framework/web/test/client/document.runtime.test.tsx`
 * established: assign onto `globalThis`, restore in `afterEach`.
 */

/** One event the fakes deliver to their listeners. */
interface FakeEvent {
  type: string;
  detail?: unknown;
  target?: unknown;
}

type Listener = (event: FakeEvent) => void;

/** An `EventTarget` that records its listeners so a test can fire them. */
class FakeEventTarget {
  readonly listeners = new Map<string, Set<Listener>>();

  addEventListener(type: string, handler: Listener): void {
    const bucket = this.listeners.get(type) ?? new Set<Listener>();
    bucket.add(handler);
    this.listeners.set(type, bucket);
  }

  removeEventListener(type: string, handler: Listener): void {
    this.listeners.get(type)?.delete(handler);
  }

  dispatchEvent(event: FakeEvent): boolean {
    for (const handler of [...(this.listeners.get(event.type) ?? [])]) {
      handler(event);
    }
    return true;
  }

  /** How many listeners are registered, all types together. */
  count(): number {
    let total = 0;
    for (const bucket of this.listeners.values()) {
      total += bucket.size;
    }
    return total;
  }
}

/** The minimum of `Element` the click delegate touches. */
export class FakeElement {
  readonly attributes: { name: string; value: string }[];

  constructor(attributes: Record<string, string>) {
    this.attributes = Object.entries(attributes).map(([name, value]) => ({ name, value }));
  }

  getAttribute(name: string): string | null {
    return this.attributes.find((attribute) => attribute.name === name)?.value ?? null;
  }

  closest(selector: string): FakeElement | null {
    return selector === '[data-track]' && this.getAttribute('data-track') !== null ? this : null;
  }
}

/** A `Storage` that a test can make throw, the way private mode does. */
interface FakeStorage {
  readonly entries: Map<string, string>;
  /** Makes every operation throw from now on. */
  breaks(): void;
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

function createStorage(): FakeStorage {
  const entries = new Map<string, string>();
  let broken = false;
  const guard = (): void => {
    if (broken) {
      throw new Error('storage is disabled');
    }
  };
  return {
    entries,
    breaks(): void {
      broken = true;
    },
    getItem(key: string): string | null {
      guard();
      return entries.get(key) ?? null;
    },
    setItem(key: string, value: string): void {
      guard();
      entries.set(key, value);
    },
    removeItem(key: string): void {
      guard();
      entries.delete(key);
    },
  };
}

/** One observed `fetch`. */
interface FetchCall {
  url: string;
  init: RequestInit;
}

/** One observed `navigator.sendBeacon`. */
interface BeaconCall {
  url: string;
  type: string;
  blob: Blob;
}

/** How the fake browser is set up for one test. */
interface FakeBrowserOptions {
  pathname?: string;
  search?: string;
  referrer?: string;
  innerWidth?: number;
  language?: string;
  /** Install `navigator.sendBeacon`; the boolean is what it returns. */
  beacon?: boolean;
  /**
   * Attributes of the one element `document.querySelector` answers with, as
   * the middleware's injected script tag would carry them.
   */
  injectedTag?: Record<string, string>;
}

/** Everything a test drives or asserts on. */
export interface FakeBrowser {
  window: FakeEventTarget & { location: { pathname: string; search: string }; innerWidth: number };
  document: FakeEventTarget & {
    referrer: string;
    visibilityState: 'visible' | 'hidden';
    querySelector(selector: string): FakeElement | null;
  };
  localStorage: FakeStorage;
  sessionStorage: FakeStorage;
  fetchCalls: FetchCall[];
  beaconCalls: BeaconCall[];
  /** Statuses (or errors) `fetch` answers, in order; 202 once exhausted. */
  responses: (number | Error)[];
  /** What `sendBeacon` returns. */
  beaconResult: boolean;
  /** The value `performance.now()` reports; advance it to move engagement. */
  clock: number;
  /** Fires a `putnami:navigation` event on the fake window. */
  navigate(detail: Record<string, unknown>): void;
  /** Fires a click whose target is `element`. */
  click(element: FakeElement): void;
  /** Sets `document.visibilityState` and fires `visibilitychange`. */
  visibility(state: 'visible' | 'hidden'): void;
  /** Removes every fake and restores what was there before. */
  uninstall(): void;
}

interface Saved {
  window: unknown;
  document: unknown;
  navigator: unknown;
  localStorage: unknown;
  sessionStorage: unknown;
  fetch: unknown;
  performance: unknown;
}

/**
 * Installs the fake browser on `globalThis`.
 *
 * @param options - Location, referrer, viewport, language, beacon support.
 * @returns The handle the test drives and asserts on.
 */
export function installFakeBrowser(options: FakeBrowserOptions = {}): FakeBrowser {
  const globals = globalThis as Record<string, unknown>;
  const saved: Saved = {
    window: globals['window'],
    document: globals['document'],
    navigator: globals['navigator'],
    localStorage: globals['localStorage'],
    sessionStorage: globals['sessionStorage'],
    fetch: globals['fetch'],
    performance: globals['performance'],
  };

  const windowTarget = Object.assign(new FakeEventTarget(), {
    location: { pathname: options.pathname ?? '/', search: options.search ?? '' },
    innerWidth: options.innerWidth ?? 1280,
  });
  // The injected tag is looked up by attribute selector, so the fake matches
  // on the attribute the selector names rather than parsing CSS.
  const injected = options.injectedTag ? new FakeElement(options.injectedTag) : undefined;
  const documentTarget = Object.assign(new FakeEventTarget(), {
    referrer: options.referrer ?? '',
    visibilityState: 'visible' as 'visible' | 'hidden',
    querySelector(selector: string): FakeElement | null {
      const attribute = /^script\[([a-z-]+)\]$/.exec(selector)?.[1];
      if (!attribute || !injected || injected.getAttribute(attribute) === null) {
        return null;
      }
      return injected;
    },
  });

  const fake: FakeBrowser = {
    window: windowTarget,
    document: documentTarget,
    localStorage: createStorage(),
    sessionStorage: createStorage(),
    fetchCalls: [],
    beaconCalls: [],
    responses: [],
    beaconResult: true,
    clock: 0,
    navigate(detail: Record<string, unknown>): void {
      windowTarget.dispatchEvent({ type: 'putnami:navigation', detail });
    },
    click(element: FakeElement): void {
      documentTarget.dispatchEvent({ type: 'click', target: element });
    },
    visibility(state: 'visible' | 'hidden'): void {
      documentTarget.visibilityState = state;
      documentTarget.dispatchEvent({ type: 'visibilitychange' });
    },
    uninstall(): void {
      for (const [key, value] of Object.entries(saved)) {
        globals[key] = value;
      }
    },
  };

  const navigator: Record<string, unknown> = { language: options.language ?? 'en-US' };
  if (options.beacon) {
    // `sendBeacon` is synchronous in a browser and answers a boolean, so the
    // blob is kept as-is and the test awaits its text when it needs the body.
    navigator['sendBeacon'] = (url: string, blob: Blob): boolean => {
      fake.beaconCalls.push({ url, type: blob.type, blob });
      return fake.beaconResult;
    };
  }

  globals['window'] = windowTarget;
  globals['document'] = documentTarget;
  globals['navigator'] = navigator;
  globals['localStorage'] = fake.localStorage;
  globals['sessionStorage'] = fake.sessionStorage;
  globals['performance'] = { now: () => fake.clock };
  globals['fetch'] = (url: string, init: RequestInit): Promise<Response> => {
    fake.fetchCalls.push({ url: String(url), init });
    const next = fake.responses.shift() ?? 202;
    if (next instanceof Error) {
      return Promise.reject(next);
    }
    return Promise.resolve(new Response(null, { status: next }));
  };

  return fake;
}

/** The bootstrap `window.__putnamiBootstrap.analytics` would carry. */
export function fakeBootstrap(overrides: Partial<ClientBootstrap> = {}): ClientBootstrap {
  return {
    pv: '0192f0c0-1234-7abc-8def-0123456789ab',
    route: '/docs/[slug]',
    endpoint: '/_putnami/analytics/events',
    app: 'demo',
    env: 'test',
    version: null,
    declared: ['signup_click', 'search'],
    ...overrides,
  };
}
