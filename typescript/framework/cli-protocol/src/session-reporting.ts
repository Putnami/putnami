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

/**
 * Session reporting v2 is v1 opened by a handshake that hands the reporter the
 * run credential of a hosted run (--credential-fd). The engine sends
 * initialize, which carries no credential; only after a reporter answers it
 * with SESSION_REPORTING_CREDENTIAL_VERSION does the engine send authenticate,
 * which carries the credential. A v1 reporter rejects initialize, whose version
 * and members it does not know, and so never receives the credential. Chunks
 * and acknowledgements carry SESSION_REPORTING_VERSION in every stream, and an
 * engine without a run credential sends no handshake.
 */
export const SESSION_REPORTING_CREDENTIAL_VERSION = 2;
export const SESSION_REPORTING_OP_INITIALIZE = 'initialize';
export const SESSION_REPORTING_OP_AUTHENTICATE = 'authenticate';
/**
 * Bounds, in UTF-8 bytes, the run credential that authenticate carries
 * (validSessionReportingCredential). The schema's maxLength holds the same
 * number in characters, a looser bound for a multi-byte credential.
 */
export const SESSION_REPORTING_MAX_CREDENTIAL_BYTES = 16_384;

export type SessionReportingOp = typeof SESSION_REPORTING_OP_INITIALIZE | typeof SESSION_REPORTING_OP_AUTHENTICATE;

/** One engine line of the v2 handshake. Only authenticate carries runCredential. */
export interface SessionReportingHandshake {
  protocolVersion: 2;
  op: SessionReportingOp;
  runCredential?: string;
}

/** The reporter's answer to one handshake line. A refusal carries a machine code. */
export interface SessionReportingHandshakeResult {
  protocolVersion: 2;
  op: SessionReportingOp;
  ok: boolean;
  code?: string;
}

const handshakeOps: string[] = [SESSION_REPORTING_OP_INITIALIZE, SESSION_REPORTING_OP_AUTHENTICATE];

/** The characters Go's unicode.IsSpace reports; a run credential holds none. */
const credentialSpace = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]/u;

/**
 * Reports whether credential is a well-formed run credential: non-empty, at
 * most SESSION_REPORTING_MAX_CREDENTIAL_BYTES UTF-8 bytes, and free of every
 * character Go's unicode.IsSpace reports. Twin of ValidSessionReportingCredential.
 */
export function validSessionReportingCredential(credential: unknown): credential is string {
  return (
    typeof credential === 'string' &&
    credential !== '' &&
    new TextEncoder().encode(credential).length <= SESSION_REPORTING_MAX_CREDENTIAL_BYTES &&
    !credentialSpace.test(credential)
  );
}

function parseHandshakeLine(line: string, required: string[], optional: string[]): Record<string, unknown> {
  if (new TextEncoder().encode(line).length > SESSION_REPORTING_LINE_BYTES)
    throw new Error('reporting line exceeds limit');
  const value: unknown = JSON.parse(line);
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error('invalid reporting JSON');
  const v = value as Record<string, unknown>;
  if (
    required.some((k) => v[k] === undefined) ||
    Object.keys(v).some((k) => v[k] === null || (!required.includes(k) && !optional.includes(k)))
  )
    throw new Error('invalid reporting fields');
  if (v['protocolVersion'] !== SESSION_REPORTING_CREDENTIAL_VERSION)
    throw new Error('invalid reporting handshake version');
  if (typeof v['op'] !== 'string' || !handshakeOps.includes(v['op']))
    throw new Error('invalid reporting handshake operation');
  return v;
}

/** Strictly decodes one engine handshake line. Twin of ParseSessionReportingHandshake. */
export function parseSessionReportingHandshake(line: string): SessionReportingHandshake {
  const v = parseHandshakeLine(line, ['protocolVersion', 'op'], ['runCredential']);
  if (v['op'] === SESSION_REPORTING_OP_INITIALIZE) {
    if (v['runCredential'] !== undefined) throw new Error('reporting initialize carries a credential');
  } else if (!validSessionReportingCredential(v['runCredential'])) throw new Error('invalid reporting credential');
  return v as unknown as SessionReportingHandshake;
}

/** Strictly decodes one reporter answer to a handshake line. Twin of ParseSessionReportingHandshakeResult. */
export function parseSessionReportingHandshakeResult(line: string): SessionReportingHandshakeResult {
  const v = parseHandshakeLine(line, ['protocolVersion', 'op', 'ok'], ['code']);
  if (
    typeof v['ok'] !== 'boolean' ||
    (v['ok'] && v['code'] !== undefined) ||
    (v['code'] !== undefined && (typeof v['code'] !== 'string' || !whole(/^[a-z][a-z0-9_]{0,63}/, v['code']))) ||
    (!v['ok'] && v['code'] === undefined)
  )
    throw new Error('invalid reporting handshake status');
  return v as unknown as SessionReportingHandshakeResult;
}

/** Reports whether result answers handshake: the same version and operation, whatever its status. */
export function sessionReportingHandshakeResultAnswers(
  result: SessionReportingHandshakeResult,
  handshake: SessionReportingHandshake,
): boolean {
  return result.protocolVersion === handshake.protocolVersion && result.op === handshake.op;
}
