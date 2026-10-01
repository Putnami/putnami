import { describe, expect, it } from 'bun:test';
import type { ClientContractOperation, ClientSchema } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import {
  ClientRequestEncodingError,
  ClientResponseContractError,
  decodeJsonBody,
  encodeJsonBody,
} from '../../src/runtime';
import { decodeFrameworkError } from '../../src/runtime/errors';

const FEATURE = 'typescript/service-clients';
const REQUIREMENT = 'additive-responses';

// A response a client was generated from: every object is closed, one is
// reached through a $ref and one through array items.
const schemas: Record<string, ClientSchema> = {
  Owner: {
    type: 'object',
    properties: { name: { type: 'string', minLength: 1 } },
    required: ['name'],
    additionalProperties: false,
  },
};
const item: ClientSchema = {
  type: 'object',
  properties: {
    id: { type: 'string' },
    count: { type: 'integer', format: 'int32' },
    owner: { $ref: '#/components/schemas/Owner' },
    tags: {
      type: 'array',
      items: {
        type: 'object',
        properties: { label: { type: 'string' } },
        required: ['label'],
        additionalProperties: false,
      },
    },
  },
  required: ['id', 'owner'],
  additionalProperties: false,
};

function closedObject(required: boolean, ...names: string[]): ClientSchema {
  return {
    type: 'object',
    properties: Object.fromEntries(names.map((name) => [name, { type: 'string' } as ClientSchema])),
    ...(required ? { required: names } : {}),
    additionalProperties: false,
  };
}

describe('additive responses', () => {
  specTest(
    'drops a response property the client does not declare, at every depth',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-response-property-the-client-does-not-declare-is-dropped' },
    () => {
      const decoded = decodeJsonBody(
        '{"id":"item-1","count":3,"addedTop":{"nested":true},' +
          '"owner":{"name":"ada","addedNested":"x"},' +
          '"tags":[{"label":"a","addedInItem":[1,2]},{"label":"b"}]}',
        item,
        schemas,
      );
      expect(decoded).toEqual({
        id: 'item-1',
        count: 3,
        owner: { name: 'ada' },
        tags: [{ label: 'a' }, { label: 'b' }],
      });
      expect(JSON.stringify(decoded)).not.toContain('added');
    },
  );

  specTest(
    'still validates every declared response property',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-declared-response-property-is-still-validated' },
    () => {
      for (const body of [
        '{"id":42,"owner":{"name":"ada"},"added":1}',
        '{"owner":{"name":"ada"},"added":1}',
        '{"id":"i","owner":{"added":1}}',
        '{"id":"i","owner":{"name":"","added":1}}',
        '{"id":"i","owner":{"name":"ada"},"count":1.5}',
        '{"id":"i","owner":{"name":"ada"},"tags":[{"label":true}]}',
        '{"id":"i","owner":{"name":"ada"},"tags":{"label":"a"}}',
        '["id","owner"]',
      ]) {
        expect(() => decodeJsonBody(body, item, schemas)).toThrow(ClientResponseContractError);
      }
    },
  );

  specTest(
    'still refuses a request property the contract does not declare',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-request-property-the-contract-does-not-declare-is-still-refused',
    },
    () => {
      expect(() => encodeJsonBody({ id: 'item-1', owner: { name: 'ada' }, added: true }, item, schemas)).toThrow(
        ClientRequestEncodingError,
      );
      expect(() => encodeJsonBody({ id: 'item-1', owner: { name: 'ada', added: true } }, item, schemas)).toThrow(
        ClientRequestEncodingError,
      );
    },
  );

  it('selects a union variant as sent first, then after dropping added properties', () => {
    const union: ClientSchema = { oneOf: [closedObject(true, 'a'), closedObject(true, 'b')] };
    expect(decodeJsonBody('{"a":"x","added":1}', union)).toEqual({ a: 'x' });
    // Once dropped properties let two variants match, the value is ambiguous.
    const ambiguous: ClientSchema = { oneOf: [closedObject(true, 'a'), closedObject(false, 'b')] };
    expect(decodeJsonBody('{"a":"x"}', ambiguous)).toEqual({ a: 'x' });
    expect(() => decodeJsonBody('{"a":"x","added":1}', ambiguous)).toThrow(ClientResponseContractError);
  });

  specTest(
    'drops an error-details property the client does not declare before it reaches the error',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-response-property-the-client-does-not-declare-is-dropped' },
    () => {
      const details = {
        field: 'name',
        note: 'sent s3cr3t',
        addedHint: 'plain',
        addedEcho: 's3cr3t',
        addedObject: { k: 'v' },
      };
      const operation: ClientContractOperation = {
        stream: 'unary',
        transports: [{ protocol: 'rest-json', path: '/items', encoding: 'json' }],
        security: { alternatives: [{ allOf: [] }] },
        errors: [{ status: 400, code: 'errors.invalid', schema: closedObject(false, 'field', 'note') }],
        idempotency: { kind: 'safe' },
      };
      const error = decodeFrameworkError({
        service: 'inventory',
        method: 'putItem',
        status: 400,
        payload: { code: 'errors.invalid', error: 'Bad Request', message: 'rejected', details },
        detailsPayload: details,
        operation,
        secrets: ['s3cr3t'],
      });
      expect(error.code).toBe('errors.invalid');
      expect(error.details).toEqual({ field: 'name', note: 'sent [REDACTED]' });
    },
  );
});
