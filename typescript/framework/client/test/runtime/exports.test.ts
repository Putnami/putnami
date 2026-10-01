import { describe, expect, test } from 'bun:test';
import * as generatorEntry from '../../src/generator/index';
import * as rootEntry from '../../src/index';
import { computeSpecHash as runtimeHash } from '../../src/runtime/spec-hash';
import { computeSpecHash as generatorHash } from '../../src/generator/string-utils';

describe('public API surface split', () => {
  test('runtime root export does NOT include the build-time generator', () => {
    const root = rootEntry as Record<string, unknown>;
    expect(root.clientGenerator).toBeUndefined();
    expect(root.generateTypeScriptClient).toBeUndefined();
    expect(root.readOpenApiSpec).toBeUndefined();
    expect(root.readProtoSpec).toBeUndefined();
    expect(root.ClientGeneratorPlugin).toBeUndefined();
  });

  test('runtime root export still includes the runtime API', () => {
    const root = rootEntry as Record<string, unknown>;
    expect(root.BaseClient).toBeDefined();
    expect(root.ClientBuilder).toBeDefined();
    expect(root.HttpTransport).toBeDefined();
    expect(root.circuitBreakerInterceptor).toBeDefined();
    expect(root.authInterceptor).toBeDefined();
    expect(root.resolveServiceUrl).toBeDefined();
  });

  test('generator subpath exposes the build-time API', () => {
    const gen = generatorEntry as Record<string, unknown>;
    expect(gen.clientGenerator).toBeDefined();
    expect(gen.generateTypeScriptClient).toBeDefined();
    expect(gen.readOpenApiSpec).toBeDefined();
    expect(gen.readProtoSpec).toBeDefined();
  });
});

describe('spec hash portability', () => {
  test('runtime (Web Crypto) and generator (node:crypto) produce identical hashes', async () => {
    const samples = ['', 'hello', '{"openapi":"3.0.3"}', 'syntax = "proto3";', 'x'.repeat(5000)];
    await Promise.all(
      samples.map(async (sample) => {
        expect(await runtimeHash(sample)).toBe(generatorHash(sample));
      }),
    );
  });

  test('runtime hash is a 16-char hex string (no Bun-only globals)', async () => {
    const hash = await runtimeHash('some spec content');
    expect(hash).toMatch(/^[0-9a-f]{16}$/);
  });
});
