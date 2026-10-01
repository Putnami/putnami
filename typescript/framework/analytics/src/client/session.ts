import { uuidv7 } from './uuidv7';

/** Where the session lives in `sessionStorage` (body §D.13). */
export const SESSION_KEY = 'putnami.analytics.session';

/** A session ends after half an hour without an event. */
export const SESSION_IDLE_MS = 1_800_000;

/** The highest sequence number the wire accepts (protocol `MaxSeq`). */
const MAX_SEQ = 1_000_000;

/** What one event needs to know about the session it belongs to. */
interface SessionStamp {
  sessionId: string;
  seq: number;
}

/** The client-owned session: it hands out a session id and a sequence number. */
export interface Session {
  /** Stamps the next event, rotating the session when it has expired. */
  next(now?: number): SessionStamp;
}

interface SessionState {
  id: string;
  lastActivity: number;
  seq: number;
}

/** The UTC day a millisecond timestamp falls in. */
function utcDay(ms: number): number {
  return Math.floor(ms / 86_400_000);
}

function parse(raw: string | null): SessionState | undefined {
  if (!raw) {
    return undefined;
  }
  try {
    const value = JSON.parse(raw) as Partial<SessionState>;
    if (typeof value?.id !== 'string' || typeof value.lastActivity !== 'number' || typeof value.seq !== 'number') {
      return undefined;
    }
    return { id: value.id, lastActivity: value.lastActivity, seq: value.seq };
  } catch {
    return undefined;
  }
}

/**
 * Creates the client-owned session (body §D.13).
 *
 * A session is a browser fact, not a server one: no cookie, no server state,
 * nothing that survives the tab. It rotates on three conditions — absent, idle
 * for thirty minutes, or a UTC day boundary crossed — because the daily
 * visitor hash rotates at midnight too, and a session that straddled the
 * boundary would be the one record able to link the two days.
 *
 * `sessionStorage` throws in private mode on some browsers. The first throw
 * switches the session to an in-memory object for the page lifetime: analytics
 * degrade, the page does not break.
 *
 * @param key - The `sessionStorage` key; overridable for tests.
 * @returns The session.
 */
export function createSession(key: string = SESSION_KEY): Session {
  let memory: SessionState | undefined;
  let inMemory = false;

  const read = (): SessionState | undefined => {
    if (inMemory) {
      return memory;
    }
    try {
      return parse(sessionStorage.getItem(key));
    } catch {
      inMemory = true;
      return memory;
    }
  };

  const write = (state: SessionState): void => {
    memory = state;
    if (inMemory) {
      return;
    }
    try {
      sessionStorage.setItem(key, JSON.stringify(state));
    } catch {
      inMemory = true;
    }
  };

  return {
    next(now: number = Date.now()): SessionStamp {
      const previous = read();
      const expired =
        !previous || now - previous.lastActivity > SESSION_IDLE_MS || utcDay(now) !== utcDay(previous.lastActivity);
      const state: SessionState = expired
        ? { id: uuidv7(now), lastActivity: now, seq: 0 }
        : { id: previous.id, lastActivity: now, seq: Math.min(previous.seq + 1, MAX_SEQ) };
      write(state);
      return { sessionId: state.id, seq: state.seq };
    },
  };
}
