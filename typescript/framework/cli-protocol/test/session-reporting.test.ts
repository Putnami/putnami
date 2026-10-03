import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/spectest';
import {
  LOG_REPORTER_COMMAND,
  LOG_REPORTER_ENV,
  LOG_REPORTER_TOKEN_ENV,
  parseSessionReportingAck,
  parseSessionReportingChunk,
  parseSessionReportingHandshake,
  parseSessionReportingHandshakeResult,
  SESSION_REPORTER_COMMAND,
  SESSION_REPORTER_ENV,
  SESSION_REPORTER_TOKEN_ENV,
  SESSION_REPORTING_CHUNK_BYTES,
  SESSION_REPORTING_CREDENTIAL_VERSION,
  SESSION_REPORTING_LINE_BYTES,
  SESSION_REPORTING_MAX_CREDENTIAL_BYTES,
  SESSION_REPORTING_OP_AUTHENTICATE,
  SESSION_REPORTING_OP_INITIALIZE,
  SESSION_REPORTING_SCHEMA_ID,
  type SessionReportingAck,
  type SessionReportingChunk,
  type SessionReportingHandshakeResult,
  sessionReportingAckMatches,
  sessionReportingArtifacts,
  sessionReportingHandshakeResultAnswers,
} from '../src/index';

const root = join(__dirname, '../../../../protocols/cli');
const corpus = JSON.parse(readFileSync(join(root, 'conformance/session-reporting.json'), 'utf8')) as {
  name: string;
  kind: string;
  valid: boolean;
  wire: unknown;
  /** The capability that sends or receives the frame, when the case names one. */
  reporter?: string;
}[];

function fixture(name: string): string {
  const item = corpus.find((entry) => entry.name === name);
  if (!item) throw new Error(`missing fixture ${name}`);
  return JSON.stringify(item.wire);
}

specTest(
  'session reporter shares the Go corpus',
  {
    feature: 'typescript/cli-machine-output',
    requirement: 'session-reporter-wire',
    check: 'shared-reporter-corpus-and-identity',
  },
  async () => {
    const reporters = new Set<string>();
    for (const item of corpus) {
      let valid = true;
      try {
        const line = JSON.stringify(item.wire);
        if (item.kind === 'handshake') parseSessionReportingHandshake(line);
        else if (item.kind === 'handshake-result') parseSessionReportingHandshakeResult(line);
        else if (item.kind === 'chunk' || item.kind === 'ack') {
          const frame = item.kind === 'chunk' ? await parseSessionReportingChunk(line) : parseSessionReportingAck(line);
          if (item.reporter !== undefined && !sessionReportingArtifacts(item.reporter).includes(frame.artifact))
            throw new Error(`${item.reporter} does not receive ${frame.artifact}`);
        } else throw new Error(`unknown corpus kind ${item.kind}`);
      } catch {
        valid = false;
      }
      expect({ name: item.name, valid }).toEqual({ name: item.name, valid: item.valid });
      if (item.valid && item.reporter !== undefined) reporters.add(item.reporter);
    }
    expect([...reporters].sort()).toEqual([LOG_REPORTER_COMMAND, SESSION_REPORTER_COMMAND]);
  },
);

test('reporter capability names and artifacts match the Go constants', () => {
  expect([SESSION_REPORTER_COMMAND, SESSION_REPORTER_ENV, SESSION_REPORTER_TOKEN_ENV]).toEqual([
    'session-reporter',
    'PUTNAMI_SESSION_REPORTER',
    'PUTNAMI_SESSION_REPORTER_TOKEN',
  ]);
  expect([LOG_REPORTER_COMMAND, LOG_REPORTER_ENV, LOG_REPORTER_TOKEN_ENV]).toEqual([
    'log-reporter',
    'PUTNAMI_LOG_REPORTER',
    'PUTNAMI_LOG_REPORTER_TOKEN',
  ]);
  expect(sessionReportingArtifacts(SESSION_REPORTER_COMMAND)).toEqual(['session.json', 'events.jsonl']);
  expect(sessionReportingArtifacts(LOG_REPORTER_COMMAND)).toEqual(['events.jsonl']);
  expect(sessionReportingArtifacts('cache-provider')).toEqual([]);
  sessionReportingArtifacts(LOG_REPORTER_COMMAND).push('session.json');
  expect(sessionReportingArtifacts(LOG_REPORTER_COMMAND)).toEqual(['events.jsonl']);
});

test('session reporter schema field sets and byte bounds', async () => {
  const schema = JSON.parse(readFileSync(join(root, 'schemas/session-reporting.json'), 'utf8'));
  expect(schema.$id).toBe(SESSION_REPORTING_SCHEMA_ID);
  const chunk = await parseSessionReportingChunk(fixture('chunk'));
  const ack: Required<SessionReportingAck> = {
    ...parseSessionReportingAck(fixture('ack')),
    code: 'unavailable',
    retryable: true,
  };
  const requiredChunk: Required<SessionReportingChunk> = chunk;
  expect(Object.keys(requiredChunk).sort()).toEqual(Object.keys(schema.$defs.chunk.properties).sort());
  expect(Object.keys(ack).sort()).toEqual(Object.keys(schema.$defs.ack.properties).sort());
  const handshake = parseSessionReportingHandshake(fixture('handshake-authenticate'));
  expect(Object.keys(handshake).sort()).toEqual(Object.keys(schema.$defs.handshake.properties).sort());
  const refused: Required<SessionReportingHandshakeResult> = {
    ...parseSessionReportingHandshakeResult(fixture('handshake-authenticate-refused')),
    code: 'unauthorized',
  };
  expect(Object.keys(refused).sort()).toEqual(Object.keys(schema.$defs.handshakeResult.properties).sort());
  expect(schema.$defs.handshake.properties.protocolVersion.const).toBe(SESSION_REPORTING_CREDENTIAL_VERSION);
  // maxLength counts characters: the schema holds the byte bound's number, and
  // only a parser bounds the UTF-8 bytes (the handshake test checks a
  // multi-byte credential).
  expect(schema.$defs.handshake.properties.runCredential.maxLength).toBe(SESSION_REPORTING_MAX_CREDENTIAL_BYTES);
  for (const size of [SESSION_REPORTING_CHUNK_BYTES, SESSION_REPORTING_CHUNK_BYTES + 1]) {
    const bytes = new Uint8Array(size).fill(120);
    const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
    const large = {
      ...chunk,
      data: btoa('x'.repeat(size)),
      sha256: Array.from(hash, (b) => b.toString(16).padStart(2, '0')).join(''),
    };
    const line = JSON.stringify(large);
    if (size === SESSION_REPORTING_CHUNK_BYTES) {
      expect(line.length).toBeLessThan(SESSION_REPORTING_LINE_BYTES);
      expect((await parseSessionReportingChunk(line)).data).toBe(large.data);
    } else await expect(parseSessionReportingChunk(line)).rejects.toThrow();
  }
});

test('session reporting acknowledgements bind every identity field', async () => {
  const chunk = await parseSessionReportingChunk(fixture('chunk'));
  const ack = parseSessionReportingAck(fixture('ack'));
  expect(sessionReportingAckMatches(ack, chunk)).toBe(true);
  for (const [key, value] of Object.entries({
    protocolVersion: 2,
    sessionId: 'other',
    artifact: 'session.json',
    offset: 1,
    sequence: 1,
    sha256: '0'.repeat(64),
    final: true,
  })) {
    expect(sessionReportingAckMatches({ ...ack, [key]: value }, chunk)).toBe(false);
  }
});

test('the v2 handshake hands the credential only in authenticate', async () => {
  expect([
    SESSION_REPORTING_CREDENTIAL_VERSION,
    SESSION_REPORTING_OP_INITIALIZE,
    SESSION_REPORTING_OP_AUTHENTICATE,
    SESSION_REPORTING_MAX_CREDENTIAL_BYTES,
  ]).toEqual([2, 'initialize', 'authenticate', 16_384]);
  const initialize = '{"protocolVersion":2,"op":"initialize"}';
  expect(parseSessionReportingHandshake(initialize)).toEqual({ protocolVersion: 2, op: 'initialize' });
  // A v1 reporter reads the engine's first line as a chunk and rejects it.
  await expect(parseSessionReportingChunk(initialize)).rejects.toThrow();
  for (const [size, valid] of [
    [SESSION_REPORTING_MAX_CREDENTIAL_BYTES, true],
    [SESSION_REPORTING_MAX_CREDENTIAL_BYTES + 1, false],
  ] as const) {
    const line = JSON.stringify({ protocolVersion: 2, op: 'authenticate', runCredential: 'x'.repeat(size) });
    expect(new TextEncoder().encode(line).length).toBeLessThan(SESSION_REPORTING_LINE_BYTES);
    if (valid) expect(parseSessionReportingHandshake(line).runCredential).toHaveLength(size);
    else expect(() => parseSessionReportingHandshake(line)).toThrow();
  }
  // The bound counts UTF-8 bytes, not UTF-16 code units.
  const wide = JSON.stringify({ protocolVersion: 2, op: 'authenticate', runCredential: '\u00e9'.repeat(8193) });
  expect(() => parseSessionReportingHandshake(wide)).toThrow();
  const authenticate = parseSessionReportingHandshake(fixture('handshake-authenticate'));
  const accepted = parseSessionReportingHandshakeResult(fixture('handshake-authenticate-accepted'));
  expect(sessionReportingHandshakeResultAnswers(accepted, authenticate)).toBe(true);
  expect(
    sessionReportingHandshakeResultAnswers(
      parseSessionReportingHandshakeResult(fixture('handshake-initialize-accepted')),
      authenticate,
    ),
  ).toBe(false);
});
