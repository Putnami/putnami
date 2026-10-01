import { describe, expect, it } from 'bun:test';
import type { ClientSchema } from '@putnami/application';
import {
  ClientRequestEncodingError,
  ClientResponseContractError,
  decodeJsonBody,
  encodeJsonBody,
} from '../../src/runtime';
import { decodeJsonValue } from '../../src/runtime/json-codec';

const schema: ClientSchema = {
  type: 'object',
  required: ['id', 'bytes', 'nullable'],
  properties: {
    id: { type: 'integer', format: 'uint64' },
    bytes: { type: 'string', format: 'byte' },
    nullable: { type: 'string', nullable: true },
    optional: { type: 'string' },
  },
};

describe('strict first-party JSON codec', () => {
  it('round-trips wide integers and bytes without Number precision loss', () => {
    const encoded = encodeJsonBody(
      { id: 18_446_744_073_709_551_615n, bytes: Uint8Array.of(0, 127, 255), nullable: null },
      schema,
    );

    expect(encoded).toBe('{"id":18446744073709551615,"bytes":"AH//","nullable":null}');
    expect(decodeJsonBody(encoded, schema)).toEqual({
      id: 18_446_744_073_709_551_615n,
      bytes: Uint8Array.of(0, 127, 255),
      nullable: null,
    });
  });

  it('keeps the bytes and the 64-bit integers a protobuf reply already decoded', () => {
    // The Connect protobuf codec hands the declared projection raw bytes and a
    // bigint for a 64-bit field; base64 and number text are the JSON forms only.
    expect(
      decodeJsonValue({ id: 18_446_744_073_709_551_615n, bytes: Uint8Array.of(0, 255), nullable: null }, schema),
    ).toEqual({ id: 18_446_744_073_709_551_615n, bytes: Uint8Array.of(0, 255), nullable: null });
    // The bounds still hold, and a bytes field is still bytes or base64 text.
    expect(() => decodeJsonValue({ id: 18_446_744_073_709_551_616n, bytes: '', nullable: null }, schema)).toThrow(
      ClientResponseContractError,
    );
    expect(() => decodeJsonValue({ id: 1n, bytes: 7, nullable: null }, schema)).toThrow(ClientResponseContractError);
  });

  it('keeps absent fields distinct from nullable fields', () => {
    expect(() => encodeJsonBody({ id: 1n, bytes: Uint8Array.of() }, schema)).toThrow(ClientRequestEncodingError);
    expect(() => decodeJsonBody('{"id":1,"bytes":""}', schema)).toThrow(ClientResponseContractError);
  });

  it('rejects malformed, trailing, duplicate, and overflowing response values', () => {
    for (const source of [
      '{"id":1,"id":2,"bytes":"","nullable":null}',
      '{"id":18446744073709551616,"bytes":"","nullable":null}',
      '{"id":1,"bytes":"","nullable":null} trailing',
    ]) {
      expect(() => decodeJsonBody(source, schema)).toThrow(ClientResponseContractError);
    }
  });

  it('validates request enums, bounds, and unambiguous unions', () => {
    expect(() => encodeJsonBody('other', { type: 'string', enum: ['known'] })).toThrow(ClientRequestEncodingError);
    expect(() => encodeJsonBody(11, { type: 'integer', maximum: 10 })).toThrow(ClientRequestEncodingError);
    expect(() =>
      encodeJsonBody(
        { value: 'same' },
        {
          oneOf: [
            { type: 'object', properties: { value: { type: 'string' } }, required: ['value'] },
            { type: 'object', properties: { value: { type: 'string' } }, required: ['value'] },
          ],
        },
      ),
    ).toThrow(/multiple union variants/);
  });

  it('uses discriminators and drops unknown response fields of closed objects', () => {
    const union: ClientSchema = {
      oneOf: [{ $ref: '#/components/schemas/Cat' }, { $ref: '#/components/schemas/Dog' }],
      discriminator: {
        propertyName: 'kind',
        mapping: { cat: '#/components/schemas/Cat', dog: '#/components/schemas/Dog' },
      },
    };
    const schemas: Record<string, ClientSchema> = {
      Cat: {
        type: 'object',
        additionalProperties: false,
        properties: { kind: { type: 'string', enum: ['cat'] }, lives: { type: 'integer' } },
        required: ['kind', 'lives'],
      },
      Dog: {
        type: 'object',
        additionalProperties: false,
        properties: { kind: { type: 'string', enum: ['dog'] }, good: { type: 'boolean' } },
        required: ['kind', 'good'],
      },
    };
    expect(decodeJsonBody('{"kind":"cat","lives":9}', union, schemas)).toEqual({ kind: 'cat', lives: 9 });
    // The discriminator selects the variant; a property it does not declare is dropped.
    expect(decodeJsonBody('{"kind":"cat","lives":9,"good":true}', union, schemas)).toEqual({ kind: 'cat', lives: 9 });
    expect(() => decodeJsonBody('{"kind":"cat","lives":"nine","good":true}', union, schemas)).toThrow(
      ClientResponseContractError,
    );
  });

  it('treats prototype-shaped JSON keys as ordinary own fields', () => {
    const hostile = JSON.parse(
      '{"type":"object","additionalProperties":false,"properties":{"__proto__":{"type":"string"},"constructor":{"type":"string"}},"required":["__proto__","constructor"]}',
    ) as ClientSchema;
    const decoded = decodeJsonBody('{"__proto__":"safe","constructor":"also-safe"}', hostile) as Record<
      string,
      unknown
    >;
    expect(Object.hasOwn(decoded, '__proto__')).toBe(true);
    const prototypeKey = '__proto__';
    expect(decoded[prototypeKey]).toBe('safe');
    expect(decoded['constructor']).toBe('also-safe');
    expect(Object.getPrototypeOf({})).toBe(Object.prototype);
    const undeclared = decodeJsonBody('{"constructor":"not-declared"}', {
      type: 'object',
      additionalProperties: false,
    }) as Record<string, unknown>;
    expect(Object.hasOwn(undeclared, 'constructor')).toBe(false);
  });

  it('does not repeat an undeclared remote property name in a response diagnostic', () => {
    const secretKey = 'bearer-secret-in-key';
    let thrown: unknown;
    try {
      decodeJsonBody(`{"${secretKey}":"value","id":1}`, {
        type: 'object',
        properties: { id: { type: 'string' } },
        additionalProperties: false,
      });
    } catch (error) {
      thrown = error;
    }
    expect(thrown).toBeInstanceOf(ClientResponseContractError);
    expect(String(thrown)).not.toContain(secretKey);
  });
});
