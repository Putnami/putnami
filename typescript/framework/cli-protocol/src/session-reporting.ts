/** Wire twin of go.putnami.dev/protocol/cli session_reporting.go. */
export const SESSION_REPORTER_COMMAND = 'session-reporter';
export const SESSION_REPORTER_ENV = 'PUTNAMI_SESSION_REPORTER';
export const SESSION_REPORTER_TOKEN_ENV = 'PUTNAMI_SESSION_REPORTER_TOKEN';
export const SESSION_REPORTING_VERSION = 1;
export const SESSION_REPORTING_CHUNK_BYTES = 65_536;
export const SESSION_REPORTING_LINE_BYTES = 98_304;
export const SESSION_REPORTING_SCHEMA_ID = 'https://putnami.dev/schemas/putnami-session-reporting.json';

/**
 * Log reporter discovery names. The log reporter speaks the same v1 wire as the
 * session reporter, restricted to the events.jsonl artifact: it never receives a
 * session.json frame, and its events final marker follows graph termination.
 */
export const LOG_REPORTER_COMMAND = 'log-reporter';
export const LOG_REPORTER_ENV = 'PUTNAMI_LOG_REPORTER';
export const LOG_REPORTER_TOKEN_ENV = 'PUTNAMI_LOG_REPORTER_TOKEN';

export type SessionReportingArtifact = 'events.jsonl' | 'session.json';

/**
 * The artifacts the reporter capability named by its reserved command receives,
 * in the order they close: session.json then events.jsonl for the session
 * reporter, events.jsonl alone for the log reporter. Any other command receives
 * nothing. Twin of SessionReportingArtifacts; the result is a fresh array.
 */
export function sessionReportingArtifacts(command: string): SessionReportingArtifact[] {
  if (command === SESSION_REPORTER_COMMAND) return ['session.json', 'events.jsonl'];
  if (command === LOG_REPORTER_COMMAND) return ['events.jsonl'];
  return [];
}

interface ReportingIdentity {
  protocolVersion: 1;
  sessionId: string;
  artifact: SessionReportingArtifact;
  offset: number;
  sequence: number;
  sha256: string;
  final: boolean;
}

export interface SessionReportingChunk extends ReportingIdentity {
  /** Canonical base64 of original artifact bytes. Empty only for a final marker. */
  data: string;
}

export interface SessionReportingAck extends ReportingIdentity {
  ok: boolean;
  retryable?: boolean;
  code?: string;
}

const identityKeys = ['protocolVersion', 'sessionId', 'artifact', 'offset', 'sequence', 'sha256', 'final'];

function whole(pattern: RegExp, value: string): boolean {
  return pattern.exec(value)?.[0] === value;
}

function parse(line: string, required: string[], optional: string[] = []): Record<string, unknown> {
  if (new TextEncoder().encode(line).length > SESSION_REPORTING_LINE_BYTES)
    throw new Error('reporting line exceeds limit');
  const value: unknown = JSON.parse(line);
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error('invalid reporting JSON');
  const v = value as Record<string, unknown>;
  if (
    required.some((k) => v[k] === undefined || v[k] === null) ||
    Object.keys(v).some((k) => !required.includes(k) && !optional.includes(k))
  )
    throw new Error('invalid reporting fields');
  if (
    v['protocolVersion'] !== 1 ||
    typeof v['sessionId'] !== 'string' ||
    !whole(/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}/, v['sessionId']) ||
    typeof v['artifact'] !== 'string' ||
    !['events.jsonl', 'session.json'].includes(v['artifact']) ||
    !Number.isSafeInteger(v['offset']) ||
    Number(v['offset']) < 0 ||
    !Number.isSafeInteger(v['sequence']) ||
    Number(v['sequence']) < 0 ||
    typeof v['sha256'] !== 'string' ||
    !whole(/^[a-f0-9]{64}/, v['sha256']) ||
    typeof v['final'] !== 'boolean'
  )
    throw new Error('invalid reporting identity');
  return v;
}

/** Validates bytes and digest using Web Crypto, usable in providers and browsers. */
export async function parseSessionReportingChunk(line: string): Promise<SessionReportingChunk> {
  const v = parse(line, [...identityKeys, 'data']);
  if (typeof v['data'] !== 'string') throw new Error('invalid reporting data');
  let binary: string;
  try {
    binary = atob(v['data']);
  } catch {
    throw new Error('invalid reporting base64');
  }
  if (
    btoa(binary) !== v['data'] ||
    binary.length > SESSION_REPORTING_CHUNK_BYTES ||
    v['final'] !== (binary.length === 0)
  )
    throw new Error('invalid reporting chunk size or final marker');
  const bytes = Uint8Array.from(binary, (c) => c.charCodeAt(0));
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
  const hex = Array.from(digest, (b) => b.toString(16).padStart(2, '0')).join('');
  if (hex !== v['sha256']) throw new Error('reporting chunk digest mismatch');
  return v as unknown as SessionReportingChunk;
}

export function parseSessionReportingAck(line: string): SessionReportingAck {
  const v = parse(line, [...identityKeys, 'ok'], ['retryable', 'code']);
  if (
    typeof v['ok'] !== 'boolean' ||
    (v['retryable'] !== undefined && typeof v['retryable'] !== 'boolean') ||
    (v['ok'] && (v['code'] !== undefined || v['retryable'] === true)) ||
    (!v['ok'] && (typeof v['code'] !== 'string' || !whole(/^[a-z][a-z0-9_]{0,63}/, v['code'])))
  )
    throw new Error('invalid reporting acknowledgement status');
  return v as unknown as SessionReportingAck;
}

export function sessionReportingAckMatches(ack: SessionReportingAck, chunk: SessionReportingChunk): boolean {
  return identityKeys.every((key) => ack[key as keyof ReportingIdentity] === chunk[key as keyof ReportingIdentity]);
}
