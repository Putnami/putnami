import { expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ClientSchema } from '@putnami/application';
import { pageTransportSchemas, validatePageTransportSchemas } from '../../src/generator/page';

specTest(
  'shared-page-schemas-match-the-go-corpus',
  {
    feature: 'typescript/service-clients',
    requirement: 'caller-resolved-bindings',
    check: 'shared-page-schemas-match-the-go-corpus',
  },
  () => {
    const fixture = JSON.parse(
      readFileSync(
        resolve(import.meta.dir, '../../../../../protocols/clientcontract/fixtures/page/schemas.json'),
        'utf8',
      ),
    );
    expect(pageTransportSchemas()).toEqual(fixture);
    expect(() => validatePageTransportSchemas(fixture.query, fixture.envelope)).not.toThrow();
    const narrowed = structuredClone(fixture);
    narrowed.query.properties.relation.enum = ['accounts'];
    narrowed.envelope.properties.rows = { type: 'array', items: { type: 'string' } };
    expect(() => validatePageTransportSchemas(narrowed.query, narrowed.envelope)).not.toThrow();
    const mutations: ((query: ClientSchema, envelope: ClientSchema) => [ClientSchema, ClientSchema])[] = [
      (q, e) => [{ ...q, properties: { ...q.properties, cursor: { type: 'string' } } }, e],
      (q, e) => [q, { ...e, required: ['relation', 'rows'] }],
      (q, e) => [q, { ...e, properties: { ...e.properties, watermark: { type: 'number' } } }],
      (q, e) => [
        q,
        { ...e, properties: { ...e.properties, watermark: { type: 'integer', format: 'int64', minimum: -1 } } },
      ],
      (q, e) => [{ ...q, properties: { ...q.properties, limit: { type: 'integer', format: 'int32', minimum: 0 } } }, e],
      (q, e) => [q, { ...e, properties: { ...e.properties, relation: { type: 'string', nullable: true } } }],
      (q, e) => [q, { ...e, properties: { ...e.properties, rows: {} } }],
      (q, e) => [q, { ...e, nullable: true }],
    ];
    for (const mutate of mutations) {
      const { query, envelope } = pageTransportSchemas();
      expect(() => validatePageTransportSchemas(...mutate(query, envelope))).toThrow();
    }
  },
);
