import { expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/spectest';
import {
  parseSessionReportingAck,
  parseSessionReportingChunk,
  SESSION_REPORTING_CHUNK_BYTES,
  SESSION_REPORTING_CODE_FRAME_UNSUPPORTED,
  SESSION_REPORTING_FRAMING_ENV,
  SESSION_REPORTING_LARGE_FRAME_VERSION,
  SESSION_REPORTING_LINE_BYTES,
  SESSION_REPORTING_MAX_CHUNK_BYTES,
  SESSION_REPORTING_MAX_LINE_BYTES,
  SESSION_REPORTING_VERSION,
  sessionReportingAckMatches,
  sessionReportingFrameVersion,
} from '../src/index';

const root = join(__dirname, '../../../../protocols/cli');
const requirement = {
  feature: 'cli/machine-contract',
  requirement: 'a-large-frame-is-negotiated',
  check: 'the-typescript-twin-reads-version-3-frames',
};

async function frame(size: number, protocolVersion: number, artifact = 'session.json', offset = 0) {
  const bytes = new Uint8Array(size).fill(120);
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
  return {
    protocolVersion,
    sessionId: 'session',
    artifact,
    offset,
    sequence: 0,
    data: Buffer.from(bytes).toString('base64'),
    sha256: Array.from(digest, (b) => b.toString(16).padStart(2, '0')).join(''),
    final: false,
  };
}

specTest('a frame above 64 KiB is version 3, up to 4 MiB, as in Go', requirement, async () => {
  expect(sessionReportingFrameVersion(SESSION_REPORTING_CHUNK_BYTES)).toBe(SESSION_REPORTING_VERSION);
  expect(sessionReportingFrameVersion(SESSION_REPORTING_CHUNK_BYTES + 1)).toBe(SESSION_REPORTING_LARGE_FRAME_VERSION);
  for (const [size, version, valid] of [
    [SESSION_REPORTING_CHUNK_BYTES + 1, 3, true],
    [SESSION_REPORTING_MAX_CHUNK_BYTES, 3, true],
    [SESSION_REPORTING_MAX_CHUNK_BYTES + 1, 3, false],
    [SESSION_REPORTING_CHUNK_BYTES + 1, 1, false],
    [3, 3, false],
  ] as const) {
    const line = JSON.stringify(await frame(size, version));
    if (valid) {
      expect(line.length).toBeLessThanOrEqual(SESSION_REPORTING_MAX_LINE_BYTES);
      const chunk = await parseSessionReportingChunk(line);
      const ack = parseSessionReportingAck(
        JSON.stringify({ ...chunk, data: undefined, ok: true, chunkBytes: SESSION_REPORTING_MAX_CHUNK_BYTES }),
      );
      expect(ack.protocolVersion).toBe(3);
      expect(sessionReportingAckMatches(ack, chunk)).toBe(true);
    } else await expect(parseSessionReportingChunk(line)).rejects.toThrow();
  }
  // A version 1 line keeps its 96 KiB bound, padding included.
  const padded = JSON.stringify(await frame(3, 1)).replace('{', `{${' '.repeat(SESSION_REPORTING_LINE_BYTES)}`);
  await expect(parseSessionReportingChunk(padded)).rejects.toThrow();
  // The plan bound still holds for a version 3 frame.
  const late = await frame(
    SESSION_REPORTING_CHUNK_BYTES + 1,
    3,
    'plan.json',
    16_777_216 - SESSION_REPORTING_CHUNK_BYTES,
  );
  await expect(parseSessionReportingChunk(JSON.stringify(late))).rejects.toThrow();
});

specTest('an acknowledgement carries chunkBytes between 64 KiB and 4 MiB, as in Go', requirement, async () => {
  const chunk = await parseSessionReportingChunk(JSON.stringify(await frame(3, 1)));
  const ack = { ...chunk, data: undefined, ok: true };
  for (const [cap, valid] of [
    [SESSION_REPORTING_CHUNK_BYTES, true],
    [SESSION_REPORTING_MAX_CHUNK_BYTES, true],
    [SESSION_REPORTING_CHUNK_BYTES - 1, false],
    [SESSION_REPORTING_MAX_CHUNK_BYTES + 1, false],
    [0, false],
    [65_536.5, false],
    ['65536', false],
  ] as const) {
    const parse = () => parseSessionReportingAck(JSON.stringify({ ...ack, chunkBytes: cap }));
    if (valid) expect(parse().chunkBytes).toBe(Number(cap));
    else expect(parse).toThrow();
  }
  const refusal = parseSessionReportingAck(
    JSON.stringify({
      ...ack,
      ok: false,
      code: SESSION_REPORTING_CODE_FRAME_UNSUPPORTED,
      chunkBytes: SESSION_REPORTING_CHUNK_BYTES,
    }),
  );
  expect(refusal.code).toBe('frame_unsupported');
  expect(SESSION_REPORTING_FRAMING_ENV).toBe('PUTNAMI_SESSION_REPORTING_FRAMING');
  const schema = JSON.parse(readFileSync(join(root, 'schemas/session-reporting.json'), 'utf8'));
  expect(schema.$defs.ack.properties.chunkBytes.minimum).toBe(SESSION_REPORTING_CHUNK_BYTES);
  expect(schema.$defs.ack.properties.chunkBytes.maximum).toBe(SESSION_REPORTING_MAX_CHUNK_BYTES);
});
