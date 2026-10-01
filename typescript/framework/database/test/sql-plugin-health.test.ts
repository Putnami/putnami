import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { join } from 'node:path';

// Assert the /healthz probe targets the workload's declared datasource (the bug:
// it used to hardcode `default`). Stub pingDatabase to record the datasource it
// is asked to ping.
//
// The stub is applied per-test (beforeEach) and reverted (afterEach), not at
// module scope: a module-scoped mock.module persists through bun's collection
// phase and would leak into other files sharing this process. Scoping it keeps
// it contained.
const FACTORY_PATH = join(import.meta.dir, '../src/factory.ts');
const realFactory = require('../src/factory') as typeof import('../src/factory');

const { sql } = await import('../src/sql.plugin');

let pingCalls: Array<string | undefined> = [];

beforeEach(() => {
  pingCalls = [];
  mock.module(FACTORY_PATH, () => ({
    ...realFactory,
    pingDatabase: async (name?: string) => {
      pingCalls.push(name);
    },
  }));
});

afterEach(() => {
  mock.module(FACTORY_PATH, () => realFactory);
});

const signal = new AbortController().signal;

describe('sql() checkHealth', () => {
  it('pings the declared primary datasource (bare name)', async () => {
    await sql({ datasource: 'identity' }).checkHealth(signal);
    expect(pingCalls).toEqual(['identity']);
  });

  it('pings the name of the { name, schema } object form', async () => {
    await sql({ datasource: { name: 'registry_put', schema: 'registry_put' } }).checkHealth(signal);
    expect(pingCalls).toEqual(['registry_put']);
  });

  it('pings the default datasource (undefined) when none is declared — back-compatible', async () => {
    await sql().checkHealth(signal);
    expect(pingCalls).toEqual([undefined]);
  });

  it('treats a blank datasource as no primary, falling back to default', async () => {
    await sql({ datasource: '   ' }).checkHealth(signal);
    expect(pingCalls).toEqual([undefined]);
  });
});
