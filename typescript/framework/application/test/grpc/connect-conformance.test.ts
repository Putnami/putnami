import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { HttpException } from '@putnami/runtime';
import {
  type ConnectCode,
  connectCodeToGrpcNumber,
  connectCodeToHttpStatus,
  CT_CONNECT_STREAM_JSON,
  CT_CONNECT_STREAM_PROTO,
  ENVELOPE_FLAG_COMPRESSED,
  ENVELOPE_FLAG_END_STREAM,
  ENVELOPE_HEADER_BYTES,
  ENVELOPE_RESERVED_FLAGS,
  formatConnectTimeout,
  fromUnpaddedBase64,
  httpStatusToConnectCode,
  parseConnectErrorBody,
  parseConnectTimeout,
  parseEndStreamResponse,
  serializeEndStreamResponse,
  toUnpaddedBase64,
} from '../../src/grpc/connect-protocol';
import { decodeProto } from '../../src/grpc/proto-codec';
import type { ProtoFieldMeta } from '../../src/proto';
import { mapErrorToGrpcStatus, handleGrpcError } from '../../src/grpc/grpc-protocol';

/**
 * Connect protocol conformance, judged by a corpus transcribed from the
 * published specification rather than by the other half of this framework.
 *
 * `corpus.json` and its README record where each scene comes from. The two
 * halves of a first-party Connect call share `connect-protocol.ts`, so agreeing
 * with each other proves nothing; agreeing with this file is the claim.
 */

interface Corpus {
  source: { specification: string; url: string; protocolVersion: string; read: string; note: string };
  codes: { code: ConnectCode; httpStatus: number; grpcNumber: number }[];
  httpInference: { status: number; code: ConnectCode }[];
  headers: Record<string, { name: string; value?: string; grammar?: string; source?: string }>;
  contentTypes: Record<string, string>;
  timeouts: { raw: string; expect: number | null }[];
  errors: { name: string; source: string; json: unknown; expect: unknown }[];
  endStream: { name: string; source: string; json: unknown; expect: unknown }[];
  envelope: {
    headerBytes: number;
    reservedMask: number;
    flags: { bit: number; mask: number; name: string; source: string }[];
    golden: { name: string; flags: number; payload: string; payloadByteLength: number; bytesHex: string };
  };
  streams: {
    name: string;
    source: string;
    frames: { flags: number; json: unknown }[];
    expect: { outcome: 'complete' | 'error'; code?: ConnectCode; message?: string; messages: unknown[] };
  }[];
  detailValue: {
    base64: string;
    bytesHex: string;
    type: string;
    descriptor: Record<string, ProtoFieldMeta[]>;
    decoded: Record<string, unknown>;
    publishedDebug: unknown;
  };
}

const CORPUS: Corpus = JSON.parse(
  readFileSync(join(import.meta.dir, 'connect-conformance/corpus.json'), 'utf8'),
) as Corpus;

/**
 * An envelope writer and reader written from the specification's byte layout
 * alone — one flag byte, a four-byte big-endian length, then the message. It
 * shares no code with `proto-codec`'s framing, so the two can disagree.
 */
function referenceEnvelope(flags: number, payload: Uint8Array): Uint8Array {
  const frame = new Uint8Array(5 + payload.length);
  frame[0] = flags & 0xff;
  frame[1] = (payload.length >>> 24) & 0xff;
  frame[2] = (payload.length >>> 16) & 0xff;
  frame[3] = (payload.length >>> 8) & 0xff;
  frame[4] = payload.length & 0xff;
  frame.set(payload, 5);
  return frame;
}

function hex(bytes: Uint8Array): string {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
}

describe('Connect protocol conformance corpus', () => {
  it('names the specification it was transcribed from', () => {
    expect(CORPUS.source.url).toBe('https://connectrpc.com/docs/protocol/');
    expect(CORPUS.source.protocolVersion).toBe('1');
  });

  specTest(
    'covers every code the protocol defines, and no others',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-protocol-conformance',
      check: 'the-corpus-carries-the-sixteen-codes-the-specification-defines',
    },
    () => {
      expect(CORPUS.codes).toHaveLength(16);
      expect(new Set(CORPUS.codes.map((entry) => entry.code)).size).toBe(16);
    },
  );

  it('has no scene the tests below leave unread', () => {
    // Non-vacuity: every list in the corpus is enumerated by a test in this
    // file, so a scene added without a test fails here instead of passing in
    // silence.
    expect(CORPUS.errors.length).toBeGreaterThan(0);
    expect(CORPUS.endStream.length).toBeGreaterThan(0);
    expect(CORPUS.streams.length).toBeGreaterThan(0);
    expect(CORPUS.httpInference.length).toBeGreaterThan(0);
    expect(CORPUS.timeouts.length).toBeGreaterThan(0);
  });
});

describe('Connect error codes', () => {
  for (const entry of CORPUS.codes) {
    it(`${entry.code} is HTTP ${entry.httpStatus} and google.rpc.Code ${entry.grpcNumber}`, () => {
      expect(connectCodeToHttpStatus(entry.code)).toBe(entry.httpStatus);
      expect(connectCodeToGrpcNumber(entry.code)).toBe(entry.grpcNumber);
    });
  }

  for (const entry of CORPUS.httpInference) {
    it(`a bare HTTP ${entry.status} infers ${entry.code}`, () => {
      expect(httpStatusToConnectCode(entry.status)).toBe(entry.code);
    });
  }

  it('does not infer the inverse of the code table', () => {
    // The two tables differ deliberately: `not_found` ships as 404, but a bare
    // 404 written by an intermediary means the route is missing, not the
    // resource.
    expect(connectCodeToHttpStatus('not_found')).toBe(404);
    expect(httpStatusToConnectCode(404)).toBe('unimplemented');
    expect(connectCodeToHttpStatus('invalid_argument')).toBe(400);
    expect(httpStatusToConnectCode(400)).toBe('internal');
  });
});

describe('Connect Error bodies', () => {
  for (const scene of CORPUS.errors) {
    it(`${scene.name} — ${scene.source}`, () => {
      const parsed = parseConnectErrorBody(scene.json);
      if (scene.expect === null) {
        expect(parsed).toBeUndefined();
        return;
      }
      expect(parsed).toEqual(scene.expect as never);
    });
  }
});

describe('EndStreamResponse', () => {
  for (const scene of CORPUS.endStream) {
    it(`${scene.name} — ${scene.source}`, () => {
      const parsed = parseEndStreamResponse(scene.json);
      if (scene.expect === null) {
        expect(parsed).toBeUndefined();
        return;
      }
      const expected = scene.expect as { ok: boolean; code?: string; metadata?: Record<string, string[]> };
      expect(parsed).toBeDefined();
      expect(parsed?.error === undefined).toBe(expected.ok);
      if (expected.code) expect(parsed?.error?.code).toBe(expected.code as ConnectCode);
      if (expected.metadata) expect(parsed?.metadata).toEqual(expected.metadata);
    });
  }

  it('serializes a successful terminal as the empty object', () => {
    expect(serializeEndStreamResponse({})).toEqual({});
    expect(JSON.stringify(serializeEndStreamResponse({}))).toBe('{}');
  });

  it('never writes a null error member', () => {
    const serialized = serializeEndStreamResponse({ metadata: { 'acme-operation-cost': ['237'] } });
    expect(Object.hasOwn(serialized, 'error')).toBe(false);
  });
});

describe('Enveloped-Message', () => {
  const golden = CORPUS.envelope.golden;

  specTest(
    'lays out one flag byte, a four-byte big-endian length, then the message',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-protocol-conformance',
      check: 'the-envelope-header-matches-the-published-golden-frame',
    },
    () => {
      expect(ENVELOPE_HEADER_BYTES).toBe(CORPUS.envelope.headerBytes);
      const payload = new TextEncoder().encode(golden.payload);
      expect(payload.length).toBe(golden.payloadByteLength);
      expect(hex(referenceEnvelope(golden.flags, payload))).toBe(golden.bytesHex);
    },
  );

  it('assigns bit 0 to compression and bit 1 to the end of the stream', () => {
    const compressed = CORPUS.envelope.flags.find((flag) => flag.name === 'compressed');
    const endStream = CORPUS.envelope.flags.find((flag) => flag.name === 'endStream');
    expect(ENVELOPE_FLAG_COMPRESSED).toBe(compressed?.mask as number);
    expect(ENVELOPE_FLAG_END_STREAM).toBe(endStream?.mask as number);
  });

  it('reserves the six most significant bits', () => {
    expect(ENVELOPE_RESERVED_FLAGS).toBe(CORPUS.envelope.reservedMask);
    expect(ENVELOPE_RESERVED_FLAGS & ENVELOPE_FLAG_COMPRESSED).toBe(0);
    expect(ENVELOPE_RESERVED_FLAGS & ENVELOPE_FLAG_END_STREAM).toBe(0);
  });
});

describe('binary error details', () => {
  const scene = CORPUS.detailValue;

  specTest(
    'decodes the published base64 detail value to the protobuf it names',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-protocol-conformance',
      check: 'a-published-binary-error-detail-decodes-to-the-message-it-names',
    },
    () => {
      const bytes = fromUnpaddedBase64(scene.base64);
      expect(hex(bytes)).toBe(scene.bytesHex);
      const decoded = decodeProto(bytes, scene.descriptor[scene.type], scene.descriptor);
      // `retry_delay` is a `google.protobuf.Duration`; `0x3c` is 60 seconds.
      expect(decoded['retryDelay']).toEqual({ seconds: 60, nanos: 0 });
    },
  );

  it('reads the bytes rather than the published debug rendering', () => {
    // The published example pairs those bytes with `{"retryDelay": "30s"}`.
    // Clients "must not depend on data in the debug key", so the bytes win.
    expect(scene.publishedDebug).toEqual({ retryDelay: '30s' });
  });

  it('round-trips unpadded base64', () => {
    const bytes = fromUnpaddedBase64(scene.base64);
    expect(toUnpaddedBase64(bytes)).toBe(scene.base64);
    expect(toUnpaddedBase64(bytes)).not.toContain('=');
  });
});

describe('Connect-Timeout-Ms', () => {
  for (const scene of CORPUS.timeouts) {
    it(`${JSON.stringify(scene.raw)} parses to ${scene.expect === null ? 'no timeout' : scene.expect}`, () => {
      expect(parseConnectTimeout(scene.raw)).toBe((scene.expect ?? undefined) as never);
    });
  }

  it('formats within the ten-digit maximum', () => {
    expect(formatConnectTimeout(1500)).toBe('1500');
    expect(formatConnectTimeout(1.2)).toBe('2');
    expect(formatConnectTimeout(0)).toBeUndefined();
    expect(formatConnectTimeout(Number.POSITIVE_INFINITY)).toBeUndefined();
    expect(formatConnectTimeout(1e15)).toBe('9999999999');
  });

  it('is not the gRPC timeout header', () => {
    expect(CORPUS.headers['timeout'].name).toBe('Connect-Timeout-Ms');
    expect(parseConnectTimeout('100m')).toBeUndefined();
  });
});

describe('media types', () => {
  it('uses the streaming media types the specification names', () => {
    expect(CT_CONNECT_STREAM_JSON).toBe(CORPUS.contentTypes['streamJson']);
    expect(CT_CONNECT_STREAM_PROTO).toBe(CORPUS.contentTypes['streamProto']);
  });
});

describe('the provider answers a failure the way the protocol defines', () => {
  specTest(
    'writes a lower-case code and a JSON content type under a non-200 status',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-protocol-conformance',
      check: 'a-unary-failure-is-a-json-error-body-under-a-non-200-status',
    },
    async () => {
      const response = handleGrpcError({ error: () => {} }, new HttpException('Not Found', 404));
      expect(response.status).toBe(404);
      expect(response.getHeader('Content-Type')).toBe(CORPUS.contentTypes['unaryErrorContentType']);
      const body = response.rawData() as Record<string, unknown>;
      expect(parseConnectErrorBody(body)?.code).toBe('not_found');
    },
  );

  it('ships the status the protocol pairs with the code it names', () => {
    for (const status of [400, 401, 403, 404, 409, 412, 413, 429, 500, 501, 503, 504]) {
      const projection = mapErrorToGrpcStatus(new HttpException('failure', status));
      expect(projection.httpStatus).toBe(connectCodeToHttpStatus(projection.code));
      expect(projection.grpcCode).toBe(connectCodeToGrpcNumber(projection.code));
    }
  });

  specTest(
    'never writes a code outside the sixteen',
    {
      feature: 'typescript/api-contracts',
      requirement: 'connect-protocol-conformance',
      check: 'no-status-this-provider-raises-produces-an-undefined-connect-code',
    },
    () => {
      for (let status = 400; status < 600; status++) {
        const projection = mapErrorToGrpcStatus(new HttpException('failure', status));
        expect(parseConnectErrorBody({ code: projection.code }), String(status)).toBeDefined();
      }
    },
  );
});
