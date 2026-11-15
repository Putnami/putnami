import {
  type AsyncSessionStore,
  removeRequestSession,
  requestSessionData,
  writeRequestSession,
} from '../../src/session/request-session';

/** An asynchronous session store over a Map, standing in for a database in tests. */
export class AsyncMemorySessionStore implements AsyncSessionStore {
  readonly asyncSession = true as const;
  readonly rows = new Map<string, Record<string, unknown>>();
  loads = 0;
  saves = 0;
  removes = 0;

  get<T>(sessionId: string, key: string): T | undefined {
    return this.getAll<Record<string, unknown>>(sessionId)[key] as T | undefined;
  }

  set<T>(sessionId: string, key: string, value: T): void {
    this.setAll(sessionId, { ...this.getAll<Record<string, unknown>>(sessionId), [key]: value });
  }

  delete(sessionId: string, key: string): void {
    const session = this.getAll<Record<string, unknown>>(sessionId);
    delete session[key];
    this.setAll(sessionId, session);
  }

  getAll<S = Record<string, unknown>>(_sessionId: string): S {
    return requestSessionData<S>();
  }

  setAll<S>(sessionId: string, session: S): void {
    writeRequestSession(sessionId, session as Record<string, unknown>);
  }

  deleteAll(sessionId: string): void {
    removeRequestSession(sessionId);
  }

  exists(sessionId: string): boolean {
    return Object.keys(this.getAll(sessionId)).length > 0;
  }

  async loadAsync(sessionId: string): Promise<Record<string, unknown>> {
    this.loads++;
    await Promise.resolve();
    return structuredClone(this.rows.get(sessionId) ?? {});
  }

  async saveAsync(sessionId: string, data: Record<string, unknown>): Promise<void> {
    this.saves++;
    await Promise.resolve();
    this.rows.set(sessionId, structuredClone(data));
  }

  async removeAsync(sessionId: string): Promise<void> {
    this.removes++;
    await Promise.resolve();
    this.rows.delete(sessionId);
  }
}
