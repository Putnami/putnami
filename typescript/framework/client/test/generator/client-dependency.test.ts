import { afterEach, describe, expect, test } from 'bun:test';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { resolveClientDependencySpec } from '../../src/generator/client-dependency';

const roots: string[] = [];

function rootWith(pkg: unknown): string {
  const dir = mkdtempSync(join(tmpdir(), 'putnami-client-dep-'));
  writeFileSync(join(dir, 'package.json'), JSON.stringify(pkg));
  roots.push(dir);
  return dir;
}

afterEach(() => {
  for (const dir of roots.splice(0)) rmSync(dir, { recursive: true, force: true });
});

describe('resolveClientDependencySpec', () => {
  test('a workspace that holds @putnami/client as a member keeps workspace:*', () => {
    expect(resolveClientDependencySpec(rootWith({ workspaces: ['typescript/framework/client'] }))).toBe('workspace:*');
  });

  test('a workspace that takes @putnami/client from its catalog gets catalog:', () => {
    expect(resolveClientDependencySpec(rootWith({ catalog: { '@putnami/client': '0.0.0-x' } }))).toBe('catalog:');
    expect(resolveClientDependencySpec(rootWith({ catalogs: { putnami: { '@putnami/client': '0.0.0-x' } } }))).toBe(
      'catalog:',
    );
  });

  test('a root-pinned version is carried as-is', () => {
    expect(resolveClientDependencySpec(rootWith({ dependencies: { '@putnami/client': '^1.2.3' } }))).toBe('^1.2.3');
  });

  test('a missing root package.json falls back to workspace:*', () => {
    const dir = mkdtempSync(join(tmpdir(), 'putnami-client-dep-'));
    roots.push(dir);
    expect(resolveClientDependencySpec(dir)).toBe('workspace:*');
  });
});
