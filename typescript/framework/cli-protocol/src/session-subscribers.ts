/** Wire twin of go.putnami.dev/protocol/cli session_subscribers.go. */
export const SESSION_SUBSCRIBERS_FILE_NAME = 'subscribers.json';
export const SESSION_SUBSCRIBERS_VERSION = 1;
export const SESSION_SUBSCRIBERS_SCHEMA_ID = 'https://putnami.dev/schemas/putnami-session-subscribers.json';
export const SESSION_SUBSCRIBERS_MAX_BYTES = 65_536;
export const SESSION_SUBSCRIBERS_MAX = 32;

/** The closed subscriber evidence vocabulary. */
export const SUBSCRIBER_EVIDENCE = {
  delivered: 'delivered',
  partial: 'partial',
  lost: 'lost',
} as const;

export type SubscriberEvidence = (typeof SUBSCRIBER_EVIDENCE)[keyof typeof SUBSCRIBER_EVIDENCE];

/** A position in events.jsonl derived from the log itself. */
export interface SessionStreamPosition {
  /** Zero-based byte offset in events.jsonl. */
  offset: number;
  /** LF-terminated records that end at or before offset. */
  records: number;
}

/** One subscriber's delivery evidence. */
export interface SessionSubscriberEvidence {
  name: string;
  evidence: SubscriberEvidence;
  acknowledged: SessionStreamPosition;
  /** stream.records minus acknowledged.records. */
  lost: number;
}

/** subscribers.json, written beside session.json after the subscribers finish. */
export interface SessionSubscribersFile {
  protocolVersion: 1;
  sessionId: string;
  stream: SessionStreamPosition;
  subscribers: SessionSubscriberEvidence[];
}

function whole(pattern: RegExp, value: string): boolean {
  return pattern.exec(value)?.[0] === value;
}

function members(value: unknown, required: string[]): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value))
    throw new Error('invalid subscriber evidence JSON');
  const v = value as Record<string, unknown>;
  if (Object.values(v).some((member) => member === null)) throw new Error('null subscriber evidence member');
  if (required.some((key) => v[key] === undefined) || Object.keys(v).some((key) => !required.includes(key)))
    throw new Error('invalid subscriber evidence fields');
  return v;
}

function position(value: unknown): SessionStreamPosition {
  const v = members(value, ['offset', 'records']);
  const offset = v['offset'];
  const records = v['records'];
  if (
    !Number.isSafeInteger(offset) ||
    !Number.isSafeInteger(records) ||
    Number(offset) < 0 ||
    Number(records) < 0 ||
    Number(records) > Number(offset)
  )
    throw new Error('invalid subscriber evidence position');
  return { offset: Number(offset), records: Number(records) };
}

function evidence(value: unknown, stream: SessionStreamPosition): SessionSubscriberEvidence {
  const v = members(value, ['name', 'evidence', 'acknowledged', 'lost']);
  const ack = position(v['acknowledged']);
  const name = v['name'];
  const kind = v['evidence'];
  if (typeof name !== 'string' || !whole(/^[a-z][a-z0-9-]{0,63}/, name)) throw new Error('invalid subscriber name');
  if (ack.offset > stream.offset || ack.records > stream.records)
    throw new Error('acknowledged position outside the stream');
  if (!Number.isSafeInteger(v['lost']) || v['lost'] !== stream.records - ack.records)
    throw new Error('lost count disagrees with the acknowledged position');
  if (kind === SUBSCRIBER_EVIDENCE.delivered) {
    if (ack.offset !== stream.offset || ack.records !== stream.records)
      throw new Error('delivered evidence must acknowledge the whole stream');
  } else if (kind === SUBSCRIBER_EVIDENCE.partial) {
    if (ack.offset === 0) throw new Error('partial evidence must acknowledge some bytes');
  } else if (kind === SUBSCRIBER_EVIDENCE.lost) {
    if (ack.offset !== 0) throw new Error('lost evidence cannot acknowledge bytes');
  } else throw new Error('unknown evidence');
  return { name, evidence: kind, acknowledged: ack, lost: Number(v['lost']) };
}

/** Strictly parses and validates subscribers.json, exactly as the Go reader does. */
export function parseSessionSubscribersFile(text: string): SessionSubscribersFile {
  if (new TextEncoder().encode(text).length > SESSION_SUBSCRIBERS_MAX_BYTES)
    throw new Error('subscriber evidence exceeds limit');
  const v = members(JSON.parse(text), ['protocolVersion', 'sessionId', 'stream', 'subscribers']);
  const sessionId = v['sessionId'];
  if (
    v['protocolVersion'] !== SESSION_SUBSCRIBERS_VERSION ||
    typeof sessionId !== 'string' ||
    !whole(/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}/, sessionId)
  )
    throw new Error('invalid subscriber evidence identity');
  const stream = position(v['stream']);
  const list = v['subscribers'];
  if (!Array.isArray(list) || list.length === 0 || list.length > SESSION_SUBSCRIBERS_MAX)
    throw new Error('invalid subscriber evidence count');
  const subscribers = list.map((entry) => evidence(entry, stream));
  for (let i = 1; i < subscribers.length; i++) {
    const previous = subscribers[i - 1];
    const current = subscribers[i];
    if (previous && current && previous.name >= current.name)
      throw new Error('subscriber names must be unique and sorted');
  }
  return { protocolVersion: SESSION_SUBSCRIBERS_VERSION, sessionId, stream, subscribers };
}
