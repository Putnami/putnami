import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { ClientContractOperation, ClientSchema } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import type { MethodIR, SpecIR } from '../../src/generator/ir.type';
import { ClientGenerationError, readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const CORPUS = join(
  import.meta.dir,
  '..',
  '..',
  '..',
  '..',
  '..',
  'protocols',
  'clientcontract',
  'fixtures',
  'openapi',
);
const OPAQUE: ClientSchema = { 'x-putnami-json': 'any' };
const FREE_FORM: ClientSchema = { type: 'object', additionalProperties: true };

/** The shape the Go provider publishes for every Go form of an uninterpreted value. */
const AUDIT: ClientSchema = {
  type: 'object',
  properties: {
    attributes: FREE_FORM,
    value: OPAQUE,
    optional: { ...OPAQUE, description: 'absent and null stay two values' },
    trail: { type: 'array', items: OPAQUE },
    snapshot: { $ref: '#/components/schemas/Snapshot' },
  },
  required: ['attributes', 'value'],
  additionalProperties: false,
};

function operation(overrides: Partial<ClientContractOperation> = {}): ClientContractOperation {
  return {
    stream: 'unary',
    transports: [{ protocol: 'rest-json', path: '/audit', encoding: 'json' }],
    security: { alternatives: [{ allOf: [] }] },
    errors: [],
    idempotency: { kind: 'idempotent' },
    ...overrides,
  } as ClientContractOperation;
}

function echoAudit(overrides: Partial<MethodIR> = {}): MethodIR {
  const reference: ClientSchema = { $ref: '#/components/schemas/Audit' };
  return {
    name: 'echoAudit',
    operationId: 'echoAudit',
    httpMethod: 'POST',
    path: '/audit',
    request: { required: true, content: [{ mediaType: 'application/json', schema: reference }] },
    successes: [{ status: 200, description: 'ok', content: [{ mediaType: 'application/json', schema: reference }] }],
    client: operation(),
    ...overrides,
  } as MethodIR;
}

function emit(method: MethodIR, schemas: Record<string, ClientSchema> = { Audit: AUDIT, Snapshot: OPAQUE }) {
  const spec = {
    irVersion: 1,
    transport: 'http',
    contract: { protocolVersion: 1, service: { id: 'audit', audience: 'api://audit' }, credentials: {} },
    schemas,
    services: [{ name: 'AuditService', className: 'AuditClient', methods: [method] }],
  } as SpecIR;
  return generateTypeScriptClient(spec, { packageName: '@test/audit-client' });
}

describe('opaque JSON in a strict first-party contract', () => {
  specTest(
    'emits unknown for an opaque value and Record<string, unknown> for a free-form object',
    {
      feature: 'typescript/service-clients',
      requirement: 'opaque-json',
      check: 'the-emitted-types-carry-opaque-json-as-unknown',
    },
    () => {
      const types = emit(echoAudit()).find((file) => file.path.endsWith('types.ts'))?.content ?? '';
      const normalized = types.replace(/\s+/g, ' ');
      expect(normalized).toContain('export type Snapshot = unknown;');
      expect(normalized).toContain('attributes: Record<string, unknown>;');
      expect(normalized).toContain('value: unknown;');
      expect(normalized).toContain('optional?: unknown;');
      expect(normalized).toContain('trail?: Array<unknown>;');
      expect(normalized).toContain('snapshot?: Snapshot');
    },
  );

  specTest(
    'refuses an opaque value where it has no lossless form',
    {
      feature: 'typescript/service-clients',
      requirement: 'opaque-json',
      check: 'an-opaque-value-is-refused-as-a-parameter-and-on-a-connect-dispatch',
    },
    () => {
      expect(() =>
        emit(
          echoAudit({
            path: '/audit/{id}',
            parameters: [{ name: 'id', location: 'path', required: true, schema: OPAQUE }],
            client: operation({ transports: [{ protocol: 'rest-json', path: '/audit/{id}', encoding: 'json' }] }),
          }),
        ),
      ).toThrow('an opaque JSON value as parameter "id"');
      // A hand-written or TypeScript-provider spec can reach the value through
      // a component reference; the refusal is the same.
      expect(() =>
        emit(
          echoAudit({
            path: '/audit/{id}',
            parameters: [
              { name: 'id', location: 'path', required: true, schema: { $ref: '#/components/schemas/Snapshot' } },
            ],
            client: operation({ transports: [{ protocol: 'rest-json', path: '/audit/{id}', encoding: 'json' }] }),
          }),
        ),
      ).toThrow('an opaque JSON value as parameter "id"');
      expect(() =>
        emit(
          echoAudit({
            parameters: [
              {
                name: 'tag',
                location: 'query',
                required: false,
                schema: { type: 'array', items: { $ref: '#/components/schemas/Snapshot' } },
              },
            ],
          }),
        ),
      ).toThrow('an opaque JSON value as parameter "tag"');
      expect(() =>
        emit(
          echoAudit({
            parameters: [
              { name: 'trail', location: 'query', required: false, schema: { $ref: '#/components/schemas/Trail' } },
            ],
          }),
          { Audit: AUDIT, Snapshot: OPAQUE, Trail: { type: 'array', items: OPAQUE } },
        ),
      ).toThrow('an opaque JSON value as parameter "trail"');
      expect(() =>
        emit(
          echoAudit({
            client: operation({
              transports: [
                {
                  protocol: 'connect',
                  path: '/audit.v1.ApiService/EchoAudit',
                  encoding: 'json',
                  protobufMethod: '/audit.v1.ApiService/EchoAudit',
                },
              ],
            }),
          }),
        ),
      ).toThrow('an opaque JSON value');
      expect(() =>
        emit(echoAudit(), {
          Audit: { type: 'object', properties: { id: { type: 'string' } }, additionalProperties: true },
          Snapshot: OPAQUE,
        }),
      ).toThrow('mixes named properties and untyped additionalProperties');
    },
  );

  test('reads the corpus declarations in their one closed spelling', () => {
    const spec = readOpenApiSource(readFileSync(join(CORPUS, 'valid', 'full.openapi.json'), 'utf8'), {
      mode: 'firstParty',
    });
    const audit = spec.schemas?.WidgetAudit;
    expect(audit?.properties?.attributes).toEqual(FREE_FORM);
    expect(audit?.properties?.before?.['x-putnami-json']).toBe('any');
    expect(audit?.properties?.trail?.items).toEqual(OPAQUE);
  });

  test('refuses the empty schema and every other opaque spelling with the Go reader codes', () => {
    const cases: Record<string, string> = {
      'empty-schema.openapi.json': 'client_contract.invalid_schema',
      'opaque-json-map-value.openapi.json': 'client_contract.invalid_schema',
      'opaque-json-unknown-value.openapi.json': 'client_contract.invalid_enum',
      'opaque-json-with-type.openapi.json': 'client_contract.invalid_schema',
    };
    for (const [name, code] of Object.entries(cases)) {
      let thrown: unknown;
      try {
        readOpenApiSource(readFileSync(join(CORPUS, 'invalid', name), 'utf8'), { mode: 'firstParty' });
      } catch (error) {
        thrown = error;
      }
      expect(thrown, name).toBeInstanceOf(ClientGenerationError);
      expect((thrown as ClientGenerationError).contractCode, name).toBe(code);
    }
  });
});
