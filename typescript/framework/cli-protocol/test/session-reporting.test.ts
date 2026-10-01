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
  SESSION_REPORTER_COMMAND,
  SESSION_REPORTER_ENV,
  SESSION_REPORTER_TOKEN_ENV,
  SESSION_REPORTING_CHUNK_BYTES,
  SESSION_REPORTING_LINE_BYTES,
  SESSION_REPORTING_SCHEMA_ID,
  type SessionReportingAck,
  type SessionReportingChunk,
  sessionReportingAckMatches,
  sessionReportingArtifacts,
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
        const frame =
          item.kind === 'chunk'
            ? await parseSessionReportingChunk(JSON.stringify(item.wire))
            : parseSessionReportingAck(JSON.stringify(item.wire));
        if (item.reporter !== undefined && !sessionReportingArtifacts(item.reporter).includes(frame.artifact))
          throw new Error(`${item.reporter} does not receive ${frame.artifact}`);
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
