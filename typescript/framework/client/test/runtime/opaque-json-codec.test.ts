import { describe, expect, it } from 'bun:test';
import type { ClientSchema } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import {
  ClientRequestEncodingError,
  ClientResponseContractError,
  decodeJsonBody,
  encodeJsonBody,
} from '../../src/runtime';

const schemas: Record<string, ClientSchema> = { Snapshot: { 'x-putnami-json': 'any' } };
const audit: ClientSchema = {
  type: 'object',
  properties: {
    attributes: { type: 'object', additionalProperties: true },
    value: { 'x-putnami-json': 'any' },
    optional: { 'x-putnami-json': 'any' },
    snapshot: { $ref: '#/components/schemas/Snapshot' },
  },
  required: ['attributes', 'value'],
  additionalProperties: false,
};

describe('opaque JSON codec', () => {
  specTest(
    'carries any JSON value through the codec without rounding an integer',
    {
      feature: 'typescript/service-clients',
      requirement: 'opaque-json',
      check: 'the-codec-carries-opaque-json-without-rounding-an-integer',
    },
    () => {
      const wire =
        '{"attributes":{"z":{"b":1,"a":[18446744073709551616,-9007199254740993,0.5]},"n":null},"value":{"z":true,"a":"x","m":{}},"optional":null,"snapshot":[{"k":"v"},12]}';
      const decoded = decodeJsonBody(wire, audit, schemas) as Record<string, Record<string, unknown>>;
      expect(decoded.attributes?.z).toEqual({ b: 1, a: [18446744073709551616n, -9007199254740993n, 0.5] });
      expect(decoded.attributes?.n).toBeNull();
      expect(decoded.optional).toBeNull();
      expect(Object.keys(decoded.value ?? {})).toEqual(['z', 'a', 'm']);
      // Writing the decoded value back yields the same JSON text, key order and
      // wide digits included.
      expect(encodeJsonBody(decoded, audit, schemas)).toBe(wire);
      // An absent optional member stays absent in both directions.
      const absent = decodeJsonBody('{"attributes":{},"value":0}', audit, schemas) as Record<string, unknown>;
      expect(Object.hasOwn(absent, 'optional')).toBe(false);
      expect(encodeJsonBody(absent, audit, schemas)).toBe('{"attributes":{},"value":0}');
    },
  );

  it('refuses a value that is not in the JSON value model instead of choosing an encoding', () => {
    const body = (value: unknown) => ({ attributes: {}, value });
    for (const value of [
      new Date(0),
      new Map(),
      Number.NaN,
      Number.POSITIVE_INFINITY,
      () => 1,
      Symbol('s'),
      [undefined],
    ]) {
      expect(() => encodeJsonBody(body(value), audit, schemas)).toThrow(ClientRequestEncodingError);
    }
    const cyclic: Record<string, unknown> = {};
    cyclic.self = cyclic;
    expect(() => encodeJsonBody(body(cyclic), audit, schemas)).toThrow(ClientRequestEncodingError);
    // A required opaque member is still required, and a free-form object is still an object.
    expect(() => encodeJsonBody({ attributes: {} }, audit, schemas)).toThrow(ClientRequestEncodingError);
    expect(() => decodeJsonBody('{"attributes":[],"value":1}', audit, schemas)).toThrow(ClientResponseContractError);
    expect(() => decodeJsonBody('{"attributes":{},"value":1e400}', audit, schemas)).toThrow(
      ClientResponseContractError,
    );
  });
});
