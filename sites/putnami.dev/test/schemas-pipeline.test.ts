import { describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { restoreEnv } from '@putnami/utils';
import {
  SCHEMA_ORIGIN,
  type SchemaSource,
  discoverSchemaSources,
  parseSchemaId,
  planSchemaPublications,
  publishPathForId,
} from '../src/lib/schemas/publish';
import { schemas } from '../src/plugins/schemas.plugin';

// The workspace root is three levels up from sites/putnami.dev/test/.
const WORKSPACE_ROOT = join(import.meta.dir, '..', '..', '..');
const PROTOCOL_SCHEMAS = 'protocols/*/schemas/*.json';
const INFRA_AGGREGATED = 'protocols/infra/schemas/infra-aggregated.json';

// ---------------------------------------------------------------------------
// publishPathForId — $id → publish path mapping
// ---------------------------------------------------------------------------

describe('publishPathForId', () => {
  it('maps a flat $id to a flat publish path', () => {
    expect(publishPathForId(`${SCHEMA_ORIGIN}putnami-project.json`)).toBe('putnami-project.json');
  });

  it('preserves nested namespaces from the $id', () => {
    expect(publishPathForId(`${SCHEMA_ORIGIN}protocol/runtime/event.json`)).toBe('protocol/runtime/event.json');
    expect(publishPathForId(`${SCHEMA_ORIGIN}events/envelope.json`)).toBe('events/envelope.json');
  });

  it('returns null for a $id outside the schema origin', () => {
    expect(publishPathForId('https://json-schema.org/draft/2020-12/schema')).toBeNull();
    expect(publishPathForId('https://putnami.dev/other/x.json')).toBeNull();
  });

  it('returns null for a missing $id', () => {
    expect(publishPathForId(undefined)).toBeNull();
    expect(publishPathForId('')).toBeNull();
  });

  it('rejects traversing or absolute paths so a bad $id cannot escape public/schemas', () => {
    expect(publishPathForId(`${SCHEMA_ORIGIN}../evil.json`)).toBeNull();
    expect(publishPathForId(`${SCHEMA_ORIGIN}a/../../evil.json`)).toBeNull();
    expect(publishPathForId(`${SCHEMA_ORIGIN}/abs.json`)).toBeNull();
    expect(publishPathForId(SCHEMA_ORIGIN)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// parseSchemaId — reading $id out of schema text
// ---------------------------------------------------------------------------

describe('parseSchemaId', () => {
  it('extracts the $id field', () => {
    expect(parseSchemaId('{"$id":"x","title":"t"}', 'a.json')).toBe('x');
  });

  it('returns undefined when $id is absent or non-string', () => {
    expect(parseSchemaId('{"title":"t"}', 'a.json')).toBeUndefined();
    expect(parseSchemaId('{"$id":123}', 'a.json')).toBeUndefined();
  });

  it('throws a clear error on invalid JSON', () => {
    expect(() => parseSchemaId('{not json', 'broken.json')).toThrow(/broken\.json is not valid JSON/);
  });
});

// ---------------------------------------------------------------------------
// planSchemaPublications — dedupe + collision detection
// ---------------------------------------------------------------------------

describe('planSchemaPublications', () => {
  it('builds a publication per distinct $id and skips non-published files', () => {
    const sources: SchemaSource[] = [
      { file: 'a.json', id: `${SCHEMA_ORIGIN}putnami-project.json` },
      { file: 'b.json', id: 'https://json-schema.org/draft/2020-12/schema' },
      { file: 'c.json', id: undefined },
    ];
    const plan = planSchemaPublications(sources);
    expect(plan).toEqual([{ file: 'a.json', urlPath: 'putnami-project.json' }]);
  });

  it('throws when two different files declare the same $id', () => {
    const sources: SchemaSource[] = [
      { file: 'infra/schemas/infra.json', id: `${SCHEMA_ORIGIN}putnami-infra.json` },
      { file: 'infra/schemas/infra-aggregated.json', id: `${SCHEMA_ORIGIN}putnami-infra.json` },
    ];
    expect(() => planSchemaPublications(sources)).toThrow(/duplicate \$id .*putnami-infra\.json/);
  });
});

// ---------------------------------------------------------------------------
// Integration — against the real workspace protocol schemas
// ---------------------------------------------------------------------------

describe('schema publishing against the real workspace', () => {
  it('discovers and publishes every public protocol schema (regression guard against silent 404s)', () => {
    const sources = discoverSchemaSources(WORKSPACE_ROOT, {
      sources: [PROTOCOL_SCHEMAS],
      exclude: [INFRA_AGGREGATED],
    });
    const plan = planSchemaPublications(sources);
    const urlPaths = new Set(plan.map((p) => p.urlPath));

    // A representative slice covering flat + both nested namespaces.
    expect(urlPaths.has('putnami-project.json')).toBe(true);
    expect(urlPaths.has('putnami-workspace.json')).toBe(true);
    expect(urlPaths.has('putnami-infra.json')).toBe(true);
    expect(urlPaths.has('protocol/runtime/event.json')).toBe(true);
    expect(urlPaths.has('events/envelope.json')).toBe(true);

    // The SDD contract: authors reference these three $ids from
    // committed documents, so a schema that stops publishing 404s in every
    // editor at once.
    expect(urlPaths.has('putnami-features.json')).toBe(true);
    expect(urlPaths.has('putnami-feature-evidence.json')).toBe(true);
    expect(urlPaths.has('putnami-spec.json')).toBe(true);

    // Floor sanity check: the bug we fixed published zero schemas.
    expect(plan.length).toBeGreaterThanOrEqual(20);
  });

  it('publishes the per-project infra schema (not the ephemeral aggregated one) at putnami-infra.json', () => {
    const plan = planSchemaPublications(
      discoverSchemaSources(WORKSPACE_ROOT, { sources: [PROTOCOL_SCHEMAS], exclude: [INFRA_AGGREGATED] }),
    );
    const infra = plan.find((p) => p.urlPath === 'putnami-infra.json');
    expect(infra?.file.endsWith('infra/schemas/infra.json')).toBe(true);
  });

  it('fails loudly on the real infra $id collision when it is not excluded', () => {
    expect(() =>
      planSchemaPublications(discoverSchemaSources(WORKSPACE_ROOT, { sources: [PROTOCOL_SCHEMAS] })),
    ).toThrow(/duplicate \$id .*putnami-infra\.json/);
  });
});

// ---------------------------------------------------------------------------
// Plugin integration — generated schema directory lifecycle
// ---------------------------------------------------------------------------

describe('schemas plugin generated output', () => {
  it('clears stale schema files before publishing the current plan', () => {
    const previousProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
    const previousWorkspaceRoot = process.env['PUTNAMI_WORKSPACE_ROOT'];
    const tempRoot = mkdtempSync(join(tmpdir(), 'putnami-schema-plugin-'));
    const projectRoot = join(tempRoot, 'site');
    const schemasRoot = join(tempRoot, 'protocols', 'demo', 'schemas');

    try {
      mkdirSync(projectRoot, { recursive: true });
      mkdirSync(schemasRoot, { recursive: true });
      writeFileSync(join(tempRoot, 'putnami.workspace.json'), '{}');
      process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
      process.env['PUTNAMI_WORKSPACE_ROOT'] = tempRoot;

      const schemaPath = join(schemasRoot, 'demo.json');
      writeFileSync(schemaPath, JSON.stringify({ $id: `${SCHEMA_ORIGIN}old-demo.json` }));
      const plugin = schemas({ sources: ['protocols/*/schemas/*.json'] });
      plugin.generate();
      expect(existsSync(join(projectRoot, 'public', 'schemas', 'old-demo.json'))).toBe(true);

      writeFileSync(schemaPath, JSON.stringify({ $id: `${SCHEMA_ORIGIN}new-demo.json` }));
      plugin.generate();

      expect(existsSync(join(projectRoot, 'public', 'schemas', 'old-demo.json'))).toBe(false);
      expect(existsSync(join(projectRoot, '.gen', 'public', 'schemas', 'old-demo.json'))).toBe(false);
      expect(existsSync(join(projectRoot, 'public', 'schemas', 'new-demo.json'))).toBe(true);
      expect(existsSync(join(projectRoot, '.gen', 'public', 'schemas', 'new-demo.json'))).toBe(true);
    } finally {
      rmSync(tempRoot, { recursive: true, force: true });
      restoreEnv('PUTNAMI_PROJECT_ROOT', previousProjectRoot);
      restoreEnv('PUTNAMI_WORKSPACE_ROOT', previousWorkspaceRoot);
    }
  });
});
