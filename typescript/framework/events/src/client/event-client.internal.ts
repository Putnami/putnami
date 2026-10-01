import {
  EVENT_FRAME_TYPES,
  PUTNAMI_EVENTS_PROTOCOL,
  type EventCompleteFrame,
  type EventErrorFrame,
  type EventMessageFrame,
} from '../protocol';
import type { EventClientConfig, EventClientReconnectOptions } from './event-client';

// ---------------------------------------------------------------------------
// Internal frame helpers shared by the WebSocket event client.
//
// These are pure, stateless utilities split out of `event-client.ts` so the
// client itself stays focused on connection/subscription lifecycle.
// ---------------------------------------------------------------------------

type EventServerEventFrame = EventMessageFrame;
export type EventServerControlFrame = EventErrorFrame | EventCompleteFrame;

const DEFAULT_RECONNECT_MIN_DELAY_MS = 500;
const DEFAULT_RECONNECT_MAX_DELAY_MS = 10_000;

/** Convert an http(s) URL to its ws(s) equivalent; pass ws(s) URLs through. */
export function toWebSocketUrl(url: string): string {
  if (url.startsWith('ws://') || url.startsWith('wss://')) {
    return url;
  }
  if (url.startsWith('http://')) {
    return `ws://${url.slice('http://'.length)}`;
  }
  if (url.startsWith('https://')) {
    return `wss://${url.slice('https://'.length)}`;
  }
  return url;
}

/** Resolve a value that may be a plain value, a sync factory, or an async factory. */
export async function resolveMaybe<T>(value: T | (() => T | Promise<T>) | undefined): Promise<T | undefined> {
  return typeof value === 'function' ? await (value as () => T | Promise<T>)() : value;
}

export function parseFrame(raw: unknown): unknown {
  if (typeof raw !== 'string') {
    throw new Error('Event Server frames must be JSON strings.');
  }
  return JSON.parse(raw) as unknown;
}

export function isEventFrame(value: unknown): value is EventServerEventFrame {
  return (
    typeof value === 'object' &&
    value !== null &&
    (value as { protocol?: unknown }).protocol === PUTNAMI_EVENTS_PROTOCOL &&
    (value as { type?: unknown }).type === EVENT_FRAME_TYPES.event &&
    'id' in value &&
    'topic' in value &&
    'payload' in value &&
    typeof (value as { id?: unknown }).id === 'string' &&
    typeof (value as { topic?: unknown }).topic === 'string'
  );
}

export function isControlFrame(value: unknown): value is EventServerControlFrame {
  return (
    typeof value === 'object' &&
    value !== null &&
    (value as { protocol?: unknown }).protocol === PUTNAMI_EVENTS_PROTOCOL &&
    ((value as { type?: unknown }).type === EVENT_FRAME_TYPES.error ||
      (value as { type?: unknown }).type === EVENT_FRAME_TYPES.complete)
  );
}

export function resolveReconnectPolicy(
  reconnect: EventClientConfig['reconnect'],
): Required<EventClientReconnectOptions> | undefined {
  if (!reconnect) {
    return undefined;
  }
  if (reconnect === true) {
    return {
      retries: Number.POSITIVE_INFINITY,
      minDelayMs: DEFAULT_RECONNECT_MIN_DELAY_MS,
      maxDelayMs: DEFAULT_RECONNECT_MAX_DELAY_MS,
    };
  }
  return {
    retries: reconnect.retries ?? Number.POSITIVE_INFINITY,
    minDelayMs: reconnect.minDelayMs ?? DEFAULT_RECONNECT_MIN_DELAY_MS,
    maxDelayMs: reconnect.maxDelayMs ?? DEFAULT_RECONNECT_MAX_DELAY_MS,
  };
}

export function addWsListener<K extends keyof WebSocketEventMap>(
  ws: WebSocket,
  type: K,
  listener: (event: WebSocketEventMap[K]) => void,
): void {
  ws.addEventListener(type, listener);
}

export function removeWsListener<K extends keyof WebSocketEventMap>(
  ws: WebSocket,
  type: K,
  listener: (event: WebSocketEventMap[K]) => void,
): void {
  ws.removeEventListener(type, listener);
}
