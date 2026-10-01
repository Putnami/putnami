import { describe, expect, it } from 'bun:test';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  canonicalizeHttpRoutes,
  HTTP_ROUTES_ERROR_CODES,
  parseAndValidateHttpRoutes,
  serializeCanonicalHttpRoutes,
  type HttpRouteInput,
} from '../../src/http-routes';

const FIXTURES = join(__dirname, '../../../../../protocols/http-routes/fixtures');
const expectations = JSON.parse(readFileSync(join(FIXTURES, 'expectations.json'), 'utf8')) as {
  valid: string[];
  invalid: Record<string, string>;
};

describe('putnami.http-routes.v1 shared conformance', () => {
  it('accepts every shared valid fixture', () => {
    expect(jsonNames(join(FIXTURES, 'valid'))).toEqual([...expectations.valid].sort(compareStrings));
    for (const name of expectations.valid) {
      const result = parseAndValidateHttpRoutes(readFileSync(join(FIXTURES, 'valid', name), 'utf8'));
      expect(result.diagnostics, name).toEqual([]);
      expect(result.manifest, name).toBeDefined();
    }
  });

  it('rejects every shared invalid fixture with its pinned diagnostic code', () => {
    expect(jsonNames(join(FIXTURES, 'invalid'))).toEqual(Object.keys(expectations.invalid).sort(compareStrings));
    for (const [name, code] of Object.entries(expectations.invalid)) {
      const result = parseAndValidateHttpRoutes(readFileSync(join(FIXTURES, 'invalid', name), 'utf8'));
      expect(result.manifest, name).toBeUndefined();
      expect(
        result.diagnostics.map((diagnostic) => diagnostic.code),
        name,
      ).toContain(code);
    }
  });

  it('declares every fixture diagnostic in the TypeScript taxonomy', () => {
    const declared = new Set(Object.values(HTTP_ROUTES_ERROR_CODES));
    for (const code of Object.values(expectations.invalid)) expect(declared.has(code), code).toBe(true);
  });

  it('emits the byte-identical Go/TypeScript golden and digest', () => {
    const parsed = parseAndValidateHttpRoutes(readFileSync(join(FIXTURES, 'valid', 'full.json'), 'utf8'));
    if (!parsed.manifest) throw new Error(`valid fixture failed: ${JSON.stringify(parsed.diagnostics)}`);
    const serialized = serializeCanonicalHttpRoutes(parsed.manifest);
    expect(serialized.diagnostics).toEqual([]);
    expect(serialized.text).toBe(readFileSync(join(FIXTURES, 'equivalence', 'http-routes.golden.json'), 'utf8'));
    expect(serialized.manifest?.digest).toBe('sha256:b9b1df83b351439e30d8aff5c6d2ec7de61de8a9bab1444738c4c27984c1f77e');
  });

  it('canonicalizes route order, method order, and method case without mutating input', () => {
    const parsed = parseAndValidateHttpRoutes(readFileSync(join(FIXTURES, 'valid', 'full.json'), 'utf8'));
    if (!parsed.manifest) throw new Error(`valid fixture failed: ${JSON.stringify(parsed.diagnostics)}`);
    const input: HttpRouteInput[] = parsed.manifest.routes.toReversed().map((route) => ({
      ...route,
      methods: route.methods.toReversed().map((method) => method.toLowerCase()),
      provenance: { ...route.provenance },
    }));
    const before = JSON.stringify(input);
    const canonical = canonicalizeHttpRoutes(input);
    expect(canonical.diagnostics).toEqual([]);
    expect(canonical.manifest?.digest).toBe('sha256:b9b1df83b351439e30d8aff5c6d2ec7de61de8a9bab1444738c4c27984c1f77e');
    expect(JSON.stringify(input)).toBe(before);
  });
});

function jsonNames(directory: string): string[] {
  return readdirSync(directory)
    .filter((name) => name.endsWith('.json'))
    .sort(compareStrings);
}

function compareStrings(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}
