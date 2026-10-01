import { describe, expect, test } from 'bun:test';
import {
  formatConnectTimeoutMs,
  grpcCodeToConnectCode,
  GrpcStatus,
  grpcStatusCode,
  grpcStatusName,
  httpStatusToGrpcCode,
  parseConnectError,
  parseEndStreamTerminal,
} from '../../src/runtime/grpc-status';

describe('grpc status names', () => {
  test('resolves a name for every canonical code', () => {
    for (const [name, code] of Object.entries(GrpcStatus)) {
      expect(grpcStatusName(code)).toBe(name as keyof typeof GrpcStatus);
      expect(grpcStatusCode(name)).toBe(code);
    }
  });

  test('unknown numeric codes read UNKNOWN', () => {
    expect(grpcStatusName(99)).toBe('UNKNOWN');
  });

  test('unknown names resolve to undefined', () => {
    expect(grpcStatusCode('NOT_A_STATUS')).toBeUndefined();
  });

  test('round-trips every numeric status through its Connect code', () => {
    for (const [name, code] of Object.entries(GrpcStatus)) {
      if (code === GrpcStatus.OK) continue; // the protocol has no code for success
      // The two vocabularies disagree on exactly one spelling: gRPC writes
      // `CANCELLED`, Connect writes `canceled`.
      const expected = code === GrpcStatus.CANCELLED ? 'canceled' : name.toLowerCase();
      expect(grpcCodeToConnectCode(code)).toBe(expected as never);
    }
  });

  test('spells cancellation the way the protocol spells it', () => {
    expect(grpcCodeToConnectCode(GrpcStatus.CANCELLED)).toBe('canceled');
  });
});

describe('httpStatusToGrpcCode', () => {
  // The protocol's "HTTP to Error Code" table: what a client infers when the
  // response carried no readable Connect error. It is not the inverse of the
  // code-to-status table, and reading it as one is the bug this pins.
  const table: [number, number][] = [
    [400, GrpcStatus.INTERNAL],
    [401, GrpcStatus.UNAUTHENTICATED],
    [403, GrpcStatus.PERMISSION_DENIED],
    [404, GrpcStatus.UNIMPLEMENTED],
    [429, GrpcStatus.UNAVAILABLE],
    [502, GrpcStatus.UNAVAILABLE],
    [503, GrpcStatus.UNAVAILABLE],
    [504, GrpcStatus.UNAVAILABLE],
    [200, GrpcStatus.OK],
    [204, GrpcStatus.OK],
    [418, GrpcStatus.UNKNOWN],
    [500, GrpcStatus.UNKNOWN],
    [405, GrpcStatus.UNKNOWN],
    [409, GrpcStatus.UNKNOWN],
  ];

  for (const [httpStatus, grpcCode] of table) {
    test(`infers gRPC ${grpcCode} from a bare HTTP ${httpStatus}`, () => {
      expect(httpStatusToGrpcCode(httpStatus)).toBe(grpcCode as never);
    });
  }
});

describe('parseConnectError', () => {
  test('prefers the body code over the HTTP status', () => {
    const info = parseConnectError({ code: 'unimplemented', message: 'use websockets' }, 501);
    expect(info.code).toBe(GrpcStatus.UNIMPLEMENTED);
    expect(info.status).toBe('UNIMPLEMENTED');
    expect(info.connectCode).toBe('unimplemented');
    expect(info.message).toBe('use websockets');
  });

  test('carries Connect error details when present', () => {
    const details = [{ type: 'google.rpc.BadRequest', value: 'CgYKBG5hbWU' }];
    const info = parseConnectError({ code: 'invalid_argument', message: 'bad', details }, 400);
    expect(info.details).toEqual(details);
  });

  test('falls back to the inference table when the body has no code', () => {
    const info = parseConnectError({ message: 'nope' }, 404);
    expect(info.code).toBe(GrpcStatus.UNIMPLEMENTED);
    expect(info.connectCode).toBeUndefined();
    // The code is inferred, but the human-readable message is still surfaced.
    expect(info.message).toBe('nope');
  });

  test('falls back to the inference table for a non-object body', () => {
    const info = parseConnectError('plain text', 503);
    expect(info.code).toBe(GrpcStatus.UNAVAILABLE);
    expect(info.message).toBeUndefined();
    expect(info.details).toBeUndefined();
  });

  test('ignores a code the protocol does not define', () => {
    // `NOT_FOUND` is the gRPC spelling; the Connect codes are lower case, and
    // there are no user-defined ones.
    for (const code of ['NOT_A_STATUS', 'NOT_FOUND', '', null, 5]) {
      expect(parseConnectError({ code }, 403).code).toBe(GrpcStatus.PERMISSION_DENIED);
    }
  });
});

describe('parseEndStreamTerminal', () => {
  test('reads the simplest successful terminal', () => {
    expect(parseEndStreamTerminal({})).toEqual({ valid: true });
  });

  test('reads an error terminal with its message and details', () => {
    const details = [{ type: 'google.rpc.ErrorInfo', value: 'CglOT1RfRk9VTkQ' }];
    const terminal = parseEndStreamTerminal({ error: { code: 'not_found', message: 'gone', details } });
    expect(terminal.valid).toBe(true);
    expect(terminal.failure?.code).toBe(GrpcStatus.NOT_FOUND);
    expect(terminal.failure?.status).toBe('NOT_FOUND');
    expect(terminal.failure?.message).toBe('gone');
    expect(terminal.failure?.details).toEqual(details);
  });

  test('reads trailing metadata with lower-cased keys', () => {
    const terminal = parseEndStreamTerminal({ metadata: { 'Acme-Operation-Cost': ['237'] } });
    expect(terminal.valid).toBe(true);
    expect(terminal.failure).toBeUndefined();
    expect(terminal.metadata).toEqual({ 'acme-operation-cost': ['237'] });
  });

  test('refuses every terminal the protocol calls invalid', () => {
    for (const body of [{ error: null }, { error: {} }, { error: { code: null } }, 'garbage', null, []]) {
      expect(parseEndStreamTerminal(body).valid).toBe(false);
    }
  });

  test('does not read gRPC-Web trailers as a failure', () => {
    // The gRPC-Web trailer block is a different protocol; reading it as an
    // `EndStreamResponse` would report a failed stream as successful — which is
    // exactly what it does here, and why the transport refuses those bytes at
    // the frame level rather than trusting this shape.
    const terminal = parseEndStreamTerminal({ 'grpc-status': 14, 'grpc-message': 'overloaded' });
    expect(terminal.valid).toBe(true);
    expect(terminal.failure).toBeUndefined();
  });
});

describe('formatConnectTimeoutMs', () => {
  test('formats whole milliseconds, with no unit suffix', () => {
    expect(formatConnectTimeoutMs(30_000)).toBe('30000');
  });

  test('rounds fractional milliseconds up', () => {
    expect(formatConnectTimeoutMs(1500.2)).toBe('1501');
  });

  test('clamps to the ten-digit maximum the grammar allows', () => {
    expect(formatConnectTimeoutMs(1e15)).toBe('9999999999');
    expect(formatConnectTimeoutMs(9_999_999_999)).toBe('9999999999');
  });

  test('rejects non-positive and non-finite timeouts', () => {
    expect(formatConnectTimeoutMs(0)).toBeUndefined();
    expect(formatConnectTimeoutMs(-1)).toBeUndefined();
    expect(formatConnectTimeoutMs(Number.NaN)).toBeUndefined();
  });
});
