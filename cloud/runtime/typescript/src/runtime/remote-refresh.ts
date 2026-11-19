import { useLogger } from '@putnami/runtime';
import {
  configServerDurationFromEnv,
  remoteFailureDetail,
  retryableRemoteFailure,
  type RemoteAttemptFailure,
} from './remote-retry';

/** The answer of one background fetch: the new tree, or why it failed. */
export type RemoteRefreshResult<T> = { tree: T } | { failure: RemoteAttemptFailure };

interface RemoteRefreshState<T> {
  tree: T;
  loadedAt: number;
  refreshing?: Promise<void>;
  lastError?: string;
}

let now: () => number = () => Date.now();

/**
 * Replaces the clock every refresh cache reads; `undefined` restores
 * `Date.now`.
 * @internal
 */
export function setRemoteRefreshClockForTest(clock: (() => number) | undefined): void {
  now = clock ?? (() => Date.now());
}

/**
 * Reads CONFIG_SERVER_REFRESH, the period in milliseconds after which a remote
 * source refreshes its tree in the background. Unset, zero or invalid keeps
 * the first load for the life of the process.
 */
export function configServerRefreshFromEnv(): number {
  if (process.env['CONFIG_SERVER_REFRESH']?.trim() === '0') {
    return 0;
  }
  return configServerDurationFromEnv('CONFIG_SERVER_REFRESH', 0);
}

/**
 * Holds the tree a remote source loaded, per request, for every instance of
 * the source in the process.
 *
 * The first load of a request blocks. Once the period has passed, a load
 * returns the held tree at once and starts one asynchronous refresh that
 * every caller shares. A refresh that answers a tree replaces the held tree;
 * one that fails, or answers none, keeps it, warns once for each new failure, and the next refresh
 * starts once another period has passed.
 */
export class RemoteRefreshCache<T> {
  private readonly states = new Map<string, RemoteRefreshState<T>>();

  constructor(private readonly source: string) {}

  get(key: string, periodMs: number, load: () => T, refresh: () => Promise<RemoteRefreshResult<T>>): T {
    const state = this.states.get(key);
    if (!state) {
      const tree = load();
      this.states.set(key, { tree, loadedAt: now() });
      return tree;
    }
    if (!state.refreshing && now() - state.loadedAt >= periodMs) {
      state.refreshing = this.refresh(state, periodMs, refresh);
    }
    return state.tree;
  }

  /** Resolves once every refresh running now has ended. */
  async settled(): Promise<void> {
    await Promise.all([...this.states.values()].map((state) => state.refreshing));
  }

  clear(): void {
    this.states.clear();
  }

  private async refresh(
    state: RemoteRefreshState<T>,
    periodMs: number,
    refresh: () => Promise<RemoteRefreshResult<T>>,
  ): Promise<void> {
    let result: RemoteRefreshResult<T>;
    try {
      result = await refresh();
    } catch (cause) {
      result = { failure: retryableRemoteFailure(`${this.source} refresh failed`, cause) };
    }
    state.loadedAt = now();
    state.refreshing = undefined;
    if ('tree' in result && result.tree == null) {
      result = { failure: retryableRemoteFailure(`${this.source} answered no tree`) };
    }
    if ('tree' in result) {
      state.tree = result.tree;
      state.lastError = undefined;
      return;
    }
    const detail = remoteFailureDetail(result.failure);
    if (detail === state.lastError) {
      return;
    }
    state.lastError = detail;
    useLogger(this.source).warn('remote source refresh failed; keeping the configuration it holds', {
      source: this.source,
      periodMs,
      error: detail,
    });
  }
}

/** Keys a request by what it asks for, not by the bearer that signs it. */
export function remoteRefreshKey(request: { url: string; method?: string; body?: string }): string {
  return JSON.stringify({ url: request.url, method: request.method, body: request.body });
}
