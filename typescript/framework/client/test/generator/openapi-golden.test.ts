import { describe, expect, test } from 'bun:test';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { OpenApiDocument } from '@putnami/application';
import type { SpecIR } from '../../src/generator/ir.type';
import { readOpenApiSpec } from '../../src/generator/openapi-reader';

/**
 * Golden parity harness.
 *
 * One shared set of OpenAPI fixtures (`fixtures/openapi/*.openapi.json`) is
 * asserted against the expected intermediate representation
 * (`fixtures/ir/*.ir.json`). The TS reader runs against them here; the Go reader
 * (Phase 2b) consumes the *same* `*.openapi.json` inputs, so the two emitters
 * cannot drift on operation set, naming, optionality, or type mapping.
 *
 * `specHash` is excluded from the comparison: it is a derived drift token over
 * the raw document bytes, not part of the structural IR contract.
 */
const OPENAPI_DIR = join(import.meta.dir, 'fixtures', 'openapi');
const IR_DIR = join(import.meta.dir, 'fixtures', 'ir');

function fixtureNames(): string[] {
  return readdirSync(OPENAPI_DIR)
    .filter((f) => f.endsWith('.openapi.json'))
    .map((f) => f.replace('.openapi.json', ''))
    .sort();
}

function readJson<T>(path: string): T {
  return JSON.parse(readFileSync(path, 'utf8')) as T;
}

describe('openapi reader golden fixtures', () => {
  const names = fixtureNames();

  test('fixture set is non-empty', () => {
    expect(names.length).toBeGreaterThan(0);
  });

  for (const name of names) {
    test(`reads ${name} to the expected IR`, () => {
      const doc = readJson<OpenApiDocument>(join(OPENAPI_DIR, `${name}.openapi.json`));
      const expected = readJson<SpecIR>(join(IR_DIR, `${name}.ir.json`));

      const { specHash, ...actual } = readOpenApiSpec(doc);
      expect(specHash).toBeTypeOf('string');
      expect(actual).toEqual(expected);
    });
  }
});
