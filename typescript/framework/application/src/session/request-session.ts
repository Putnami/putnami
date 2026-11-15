import { tryContext, useConfig } from '@putnami/runtime';
import { cookies } from './cookies';
import { SessionConfig } from './session.config';
import { getRegisteredStore, type SessionStore } from './session.store';

/**
 * A session store whose data lives outside the request, behind asynchronous
 * reads and writes (a database, a remote cache).
 *
 * The session API (`useSession`, `setSession`, ...) is synchronous. For such a
 * store the HTTP plugin loads the session once before the middlewares run
 * ({@link loadRequestSession}) and writes it once after the response is built
 * ({@link commitRequestSession}), only when something changed it. In between,
 * the store's synchronous methods read and write the request's copy through
 * {@link requestSessionData} and {@link writeRequestSession}.
 */
export interface AsyncSessionStore extends SessionStore {
  readonly asyncSession: true;
  /** The stored session, or an empty object when the ID names none or it expired. */
  loadAsync(sessionId: string): Promise<Record<string, unknown>>;
  saveAsync(sessionId: string, data: Record<string, unknown>): Promise<void>;
  removeAsync(sessionId: string): Promise<void>;
}

/** Whether `store` keeps its sessions behind asynchronous reads and writes. */
export function isAsyncSessionStore(store: SessionStore): store is AsyncSessionStore {
  return (store as Partial<AsyncSessionStore>).asyncSession === true;
}

interface RequestSessionState {
  store: AsyncSessionStore;
  /** The ID the session was loaded or last written under. */
  sessionId?: string;
  data: Record<string, unknown>;
  dirty: boolean;
  removed: boolean;
}

type RequestSessionContext = { __requestSession?: RequestSessionState };

/** The ID cookie of a server-side session store. */
export function sessionIdCookieName(): string {
  return `${useConfig(SessionConfig).cookieName}_id`;
}

/**
 * Loads the request's session from an asynchronous store. Does nothing for a
 * synchronous store such as the cookie or memory store. A request without the
 * session ID cookie reads nothing and starts from an empty session.
 */
export async function loadRequestSession(): Promise<void> {
  const context = tryContext<RequestSessionContext>();
  if (!context || context.__requestSession) return;
  const config = useConfig(SessionConfig);
  if (config.store === 'cookie') return;
  const store = getRegisteredStore(config.store);
  if (!isAsyncSessionStore(store)) return;

  const sessionId = cookies().get(sessionIdCookieName());
  const data = sessionId ? await store.loadAsync(sessionId) : {};
  context.__requestSession = { store, sessionId, data, dirty: false, removed: false };
}

/** Writes the request's session back to its asynchronous store, when the request changed it. */
export async function commitRequestSession(): Promise<void> {
  const state = tryContext<RequestSessionContext>()?.__requestSession;
  if (!state?.dirty || !state.sessionId) return;
  state.dirty = false;
  if (state.removed) {
    await state.store.removeAsync(state.sessionId);
  } else {
    await state.store.saveAsync(state.sessionId, state.data);
  }
}

function requestSessionState(): RequestSessionState {
  const state = tryContext<RequestSessionContext>()?.__requestSession;
  if (!state) {
    throw new Error(
      'The session was not loaded for this request: an asynchronous session store serves the session API only inside an HTTP request',
    );
  }
  return state;
}

/** A copy of the request's session data, for an asynchronous store's synchronous reads. */
export function requestSessionData<S = Record<string, unknown>>(): S {
  return { ...requestSessionState().data } as S;
}

/** Replaces the request's session data under `sessionId`; the HTTP plugin writes it after the response. */
export function writeRequestSession(sessionId: string, data: Record<string, unknown>): void {
  const state = requestSessionState();
  state.sessionId = sessionId;
  state.data = { ...data };
  state.dirty = true;
  state.removed = false;
}

/** Empties the request's session under `sessionId`; the HTTP plugin deletes it after the response. */
export function removeRequestSession(sessionId: string): void {
  const state = requestSessionState();
  state.sessionId = sessionId;
  state.data = {};
  state.dirty = true;
  state.removed = true;
}
