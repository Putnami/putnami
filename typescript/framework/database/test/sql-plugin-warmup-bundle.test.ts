import { afterAll, describe, expect, it, mock } from 'bun:test';
import { registerModuleLoader } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import * as realUtils from '@putnami/utils';

// A bundled build runs where no workspace exists: looking the current project
// up throws "No workspace found". The bundle registers 'sql-loader' only when
// the app declares tables, so an app that uses sql() for a datasource alone
// (a session store, for instance) has none. Its warmup must not look the
// project up.
// Bun keeps a module mock for the rest of the process, whatever afterAll
// installs: every later test file in it would find no workspace either, and
// read a config without the project's files. The mock throws only while this
// file runs, then answers as the real function does.
// The namespace is live: copy the real functions before the mock replaces them.
const utils = { ...realUtils };
let noWorkspace = true;
mock.module('@putnami/utils', () => ({
  ...utils,
  getCurrentProject: (...args: Parameters<typeof utils.getCurrentProject>) => {
    if (noWorkspace) throw new Error('No workspace found. Please run the command `putnami workspace init`');
    return utils.getCurrentProject(...args);
  },
}));

afterAll(() => {
  noWorkspace = false;
  // The warmup set the process's primary datasource; later files expect none.
  setPrimaryDatasource(undefined);
  // Config read while no project could be found holds none of the project's
  // files; drop it so the next test file reads them.
  resetConfigLoader();
});

const { sql } = await import('../src/sql.plugin');
const { setPrimaryDatasource } = await import('../src/primary-datasource');

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
