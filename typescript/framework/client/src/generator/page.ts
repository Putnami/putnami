import type { ClientSchema, OpenApiDocument } from '@putnami/application';
import { readOpenApiSpecWithOptions } from './openapi-reader';

/** Shared snapshot query names; the owner supplies relation vocabulary and bounds. */
export const QueryParamRelation = 'relation';
export const QueryParamAfterKey = 'afterKey';
export const QueryParamLimit = 'limit';

/**
 * One shared keyset page. Only empty/absent nextKey ends a relation, never row
 * count. Watermarks are captured before rows; consumers anchor at the minimum
 * seen. ownerConfirmedAt is the authoritative owner's clock, never receipt time.
 * Generated clients preserve the int64 watermark through their exact JSON codec.
 */
export function pageTransportSchemas(): { query: ClientSchema; envelope: ClientSchema } {
  return {
    query: {
      type: 'object',
      properties: {
        relation: { type: 'string' },
        afterKey: { type: 'string' },
        limit: { type: 'integer', format: 'int32', minimum: 1 },
      },
      required: ['relation'],
    },
    envelope: {
      type: 'object',
      properties: {
        watermark: { type: 'integer', format: 'int64', minimum: 0 },
        ownerConfirmedAt: { type: 'string', format: 'date-time' },
        relation: { type: 'string' },
        rows: { 'x-putnami-json': 'any' },
        nextKey: { type: 'string' },
      },
      required: ['watermark', 'relation', 'rows'],
    },
  };
}

/** Pin resolved owner schemas to the shared shape, allowing only owner narrowing. */
export function validatePageTransportSchemas(query: ClientSchema, envelope: ClientSchema): void {
  // Reuse the strict reader for schema validity, including the owner's rows.
  readOpenApiSpecWithOptions(
    {
      openapi: '3.0.3',
      info: { title: 'Page conformance', version: '1' },
      paths: {},
      'x-putnami-client': { protocolVersion: 1, service: { id: 'page', audience: 'urn:page' }, credentials: {} },
      components: { schemas: { Query: query, Envelope: envelope } },
    } as OpenApiDocument,
    { mode: 'firstParty' },
  );
  const expected = pageTransportSchemas();
  for (const [name, actual, want] of [
    ['query', query, expected.query],
    ['envelope', envelope, expected.envelope],
  ] as const) {
    if (
      actual.type !== 'object' ||
      actual.$ref ||
      actual.nullable ||
      Object.keys(actual.properties ?? {}).length !== Object.keys(want.properties ?? {}).length
    ) {
      throw new Error(`page ${name} must declare exactly the shared properties`);
    }
    for (const required of want.required ?? []) {
      if (!actual.required?.includes(required)) throw new Error(`page ${name}.${required} must be required`);
    }
    for (const [key, shape] of Object.entries(want.properties ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
      const field = actual.properties?.[key];
      if (!field) throw new Error(`page ${name}.${key} is missing`);
      if (key === 'rows') continue;
      if (field.type !== shape.type || field.format !== shape.format || field.$ref || field.nullable) {
        throw new Error(`page ${name}.${key} has an incompatible wire type`);
      }
      if (
        shape.minimum !== undefined &&
        (field.minimum === undefined ||
          Number(typeof field.minimum === 'object' ? field.minimum.$number : field.minimum) < Number(shape.minimum))
      ) {
        throw new Error(`page ${name}.${key} weakens its minimum`);
      }
    }
  }
}
