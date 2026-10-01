import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  canonicalMigrationId,
  DEFAULT_DATASOURCE,
  type DriftReport,
  isDriftReportEmpty,
  isMigrationContributor,
  KindSQL,
  type MigrationRecord,
  sha256Hex,
  sha256HexSync,
} from '../src';

describe('canonicalMigrationId', () => {
  test('uses default datasource when empty', () => {
    expect(canonicalMigrationId(undefined, 'iam/001')).toBe(`${DEFAULT_DATASOURCE}:iam/001`);
    expect(canonicalMigrationId('', 'iam/001')).toBe(`${DEFAULT_DATASOURCE}:iam/001`);
  });

  test('preserves explicit datasource', () => {
    expect(canonicalMigrationId('analytics', 'iam/001')).toBe('analytics:iam/001');
  });
});

describe('isDriftReportEmpty', () => {
  test('true for zero report', () => {
    expect(isDriftReportEmpty({ kind: KindSQL })).toBe(true);
  });

  test('false when any drift recorded', () => {
    const hash: DriftReport = {
      kind: KindSQL,
      hashDrifts: [{ name: 'iam/001', storedHash: 'a', currentHash: 'b' }],
    };
    const pending: DriftReport = {
      kind: KindSQL,
      missingFromStore: [{ kind: KindSQL, name: 'iam/002', status: 'pending' }],
    };
    expect(isDriftReportEmpty(hash)).toBe(false);
    expect(isDriftReportEmpty(pending)).toBe(false);
  });
});

describe('sha256Hex (cross-language fixture)', () => {
  test('matches the canonical vectors used by the Go runner', async () => {
    expect(await sha256Hex('')).toBe('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
    expect(await sha256Hex('hello')).toBe('2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824');
  });
});

describe('sha256HexSync', () => {
  test('matches the canonical vectors used by the Go runner', () => {
    expect(sha256HexSync('')).toBe('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
    expect(sha256HexSync('hello')).toBe('2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824');
  });

  test('agrees with the async sha256Hex for the same input', async () => {
    for (const input of ['', 'hello', 'iam/20260520120000_create_users', 'multi\nline\tcontent']) {
      expect(sha256HexSync(input)).toBe(await sha256Hex(input));
    }
  });

  test('falls back to node:crypto when Bun.CryptoHasher is absent (non-Bun runtimes)', () => {
    // Simulate Node/Deno/browser by hiding Bun's fast path, then restore it so
    // the rest of the suite still exercises the Bun path. This guards the
    // cross-runtime contract: the helper must not throw off-Bun.
    const holder = globalThis as { Bun?: { CryptoHasher?: unknown } };
    const original = holder.Bun?.CryptoHasher;
    if (holder.Bun) holder.Bun.CryptoHasher = undefined;
    try {
      expect(sha256HexSync('hello')).toBe('2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824');
    } finally {
      if (holder.Bun) holder.Bun.CryptoHasher = original;
    }
  });
});

describe('isMigrationContributor', () => {
  test('true when migrationSources is a function', () => {
    const plugin = { name: 'iam', migrationSources: () => [] };
    expect(isMigrationContributor(plugin)).toBe(true);
  });

  test('false on plain plugins', () => {
    expect(isMigrationContributor({ name: 'http' })).toBe(false);
    expect(isMigrationContributor(null)).toBe(false);
    expect(isMigrationContributor({ migrationSources: 'not-a-function' })).toBe(false);
  });
});

describe('MigrationRecord', () => {
  test('is exported and usable as the runner result row', () => {
    const row: MigrationRecord = { kind: KindSQL, name: 'iam/001', status: 'applied' };
    expect(row.name).toBe('iam/001');
    expect(row.status).toBe('applied');
  });

  test('declares no `Record` type, which would shadow the global utility type', () => {
    // The deprecated `export type Record = MigrationRecord` alias was removed as
    // a documented pre-1.0 breaking change: importing it shadowed TypeScript's
    // global `Record<K, V>` in the consumer's module. It is a type, so nothing
    // at runtime can observe it — the guard has to read the declaration.
    const source = readFileSync(join(import.meta.dir, '..', 'src', 'types.ts'), 'utf8');
    expect(source).not.toMatch(/^export (type|interface) Record\b/m);
    expect(source).toMatch(/^export interface MigrationRecord\b/m);
  });
});
