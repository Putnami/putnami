import { afterAll, describe, expect, it, mock } from 'bun:test';
import { registerModuleLoader } from '@putnami/application';
import * as realUtils from '@putnami/utils';

// A bundled build runs where no workspace exists: looking the current project
// up throws "No workspace found". The bundle registers 'sql-loader' only when
// the app declares tables, so an app that uses sql() for a datasource alone
// (a session store, for instance) has none. Its warmup must not look the
// project up.
mock.module('@putnami/utils', () => ({
  ...realUtils,
  getCurrentProject: () => {
    throw new Error('No workspace found. Please run the command `putnami workspace init`');
  },
}));

afterAll(() => {
  mock.module('@putnami/utils', () => realUtils);
});

const { sql } = await import('../src/sql.plugin');

function owner() {
  const runners: unknown[] = [];
  const registry = { registerRunner: (runner: unknown) => runners.push(runner) };
  const module = {
    getRoot: () => ({ getMigrationRegistry: () => registry }),
    provide: () => undefined,
  };
  return { module, runners };
}

describe('sql() warmup in a bundle', () => {
  it('starts without a workspace when the bundle registers no sql-loader', async () => {
    registerModuleLoader('react-loader', async () => ({}));
    const { module, runners } = owner();

    // biome-ignore lint/suspicious/noExplicitAny: a minimal Module stand-in
    await sql({ datasource: 'sessions' }).warmup?.(module as any);

    expect(runners).toHaveLength(1);
  });
});
