import { afterEach, describe, expect, it } from 'bun:test';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  clearLoggerCache,
  createLogger,
  GeneratorHelper,
  getEnv,
  getLogger,
  readProjectConfigFile,
  restoreEnv,
} from '../src';

const tempDirs: string[] = [];

interface LoggerConsole {
  info: (...args: unknown[]) => void;
  warn: (...args: unknown[]) => void;
}

function makeTempDir(): string {
  const dir = mkdtempSync(join(tmpdir(), 'putnami-utils-server-helpers-'));
  tempDirs.push(dir);
  return dir;
}

afterEach(() => {
  while (tempDirs.length > 0) {
    const dir = tempDirs.pop();
    if (dir) {
      rmSync(dir, { recursive: true, force: true });
    }
  }
  clearLoggerCache();
});

describe('logger utils', () => {
  it('prefixes named logger output and supports unnamed logger output', () => {
    const infoCalls: unknown[][] = [];
    const warnCalls: unknown[][] = [];
    const loggerConsole = globalThis['console'] as LoggerConsole;
    const originalInfo = loggerConsole.info;
    const originalWarn = loggerConsole.warn;

    loggerConsole.info = (...args: unknown[]) => {
      infoCalls.push(args);
    };
    loggerConsole.warn = (...args: unknown[]) => {
      warnCalls.push(args);
    };

    try {
      createLogger('service').info('started', 1);
      createLogger().warn('plain');
    } finally {
      loggerConsole.info = originalInfo;
      loggerConsole.warn = originalWarn;
    }

    expect(infoCalls[0]).toEqual(['[service]', 'started', 1]);
    expect(warnCalls[0]).toEqual(['plain']);
  });

  it('caches named and default loggers and can clear cache', () => {
    const namedA = getLogger('db');
    const namedB = getLogger('db');
    const defaultA = getLogger();
    const defaultB = getLogger();

    expect(namedA).toBe(namedB);
    expect(defaultA).toBe(defaultB);

    clearLoggerCache();

    expect(getLogger('db')).not.toBe(namedA);
    expect(getLogger()).not.toBe(defaultA);
  });
});

describe('env utils', () => {
  const originalNodeEnv = process.env.NODE_ENV;
  const originalKService = process.env.K_SERVICE;

  afterEach(() => {
    restoreEnv('NODE_ENV', originalNodeEnv);
    restoreEnv('K_SERVICE', originalKService);
  });

  it('detects test and production environments', () => {
    process.env.NODE_ENV = 'test';
    delete process.env.K_SERVICE;
    expect(getEnv()).toBe('test');

    process.env.NODE_ENV = 'production';
    expect(getEnv()).toBe('production');

    process.env.NODE_ENV = 'prod';
    expect(getEnv()).toBe('production');
  });

  it('detects Cloud Run and development fallback', () => {
    delete process.env.NODE_ENV;
    process.env.K_SERVICE = 'service-name';
    expect(getEnv()).toBe('production');

    delete process.env.K_SERVICE;
    expect(getEnv()).toBe('development');
  });
});

describe('project config utils', () => {
  it('returns undefined when config file is missing', () => {
    const dir = makeTempDir();
    expect(readProjectConfigFile(dir)).toBeUndefined();
  });

  it('reads valid config and returns undefined for invalid JSON', () => {
    const dir = makeTempDir();
    const configFile = join(dir, 'putnami.json');

    writeFileSync(configFile, JSON.stringify({ name: 'demo', jobs: { build: { kind: 'command', command: 'echo' } } }));
    expect(readProjectConfigFile(dir)).toEqual({
      name: 'demo',
      jobs: { build: { kind: 'command', command: 'echo' } },
    });

    writeFileSync(configFile, '{invalid-json');
    expect(readProjectConfigFile(dir)).toBeUndefined();
  });
});

describe('generator helper', () => {
  it('normalizes imports and module names', () => {
    const helper = new GeneratorHelper('/tmp/unused.ts');

    expect(helper.normalizeImport('feature/main.ts')).toBe('./feature/main');
    expect(helper.normalizeImport('./local/module.jsx')).toBe('./local/module');
    expect(helper.normalizeImport('/absolute/path/util.tsx')).toBe('./absolute/path/util');

    expect(helper.getModuleName('feature/main.ts')).toBe('__feature_main');
    expect(helper.getModuleName('feature/main.ts', 'Suffix')).toBe('__feature_mainSuffix');
  });

  it('builds and writes generated files', () => {
    const dir = makeTempDir();
    const outputFile = join(dir, 'generated', 'index.ts');
    const helper = new GeneratorHelper(outputFile);

    const defaultImportName = helper.addImportDefault('feature/main.ts', 'Loader', '..');
    const moduleImportName = helper.addImportModule('/lib/tools.ts', '..');
    const lazyImport = helper.getLazyImport('widgets/ui.js', '..');

    expect(defaultImportName).toBe('__feature_mainLoader');
    expect(moduleImportName).toBe('__lib_tools');
    expect(lazyImport).toContain("() => import('");
    expect(lazyImport).toContain('widgets/ui');

    helper.appendHead('// generated file');
    helper.append(`export const fromDefault = ${defaultImportName};`);
    helper.append(`export const fromModule = ${moduleImportName};`);
    helper.append(`export const lazy = ${lazyImport};`);

    expect(helper.write()).toBe(outputFile);
    const firstWrite = readFileSync(outputFile, 'utf8');
    expect(firstWrite).toContain("import __feature_mainLoader from '../feature/main';");
    expect(firstWrite).toContain("import * as __lib_tools from '../lib/tools';");
    expect(firstWrite).toContain('// generated file');
    expect(firstWrite).toContain('export const lazy = () => import(');

    expect(helper.write()).toBe(outputFile);
    const secondWrite = readFileSync(outputFile, 'utf8');
    expect(secondWrite).toBe(firstWrite);
  });

  it('writes forward-slash specifiers and identifiers from Windows-separated paths', () => {
    // On Windows the generators hand the helper native paths. A backslash in a
    // specifier is an escape sequence (`\n` in `..\not-found` is a newline)
    // and is not a valid identifier character, so Bun rejects the file.
    const dir = makeTempDir();
    const outputFile = join(dir, 'generated', 'index.ts');
    const helper = new GeneratorHelper(outputFile);

    expect(helper.normalizeImport('..\\..\\src\\api\\get.ts')).toBe('../../src/api/get');
    expect(helper.getModuleName('users\\[id]\\get.ts')).toBe('__users__id__get');

    const routeModule = helper.addImportModule('..\\..\\..\\src\\api\\get.ts');
    const notFound = helper.addImportModule('not-found.tsx', '..\\..\\..\\src\\app');
    const layout = helper.addImportDefault('dashboard\\layout.tsx', 'Layout', '..\\..\\src\\app');
    const lazyPage = helper.getLazyImport('blog\\[slug]\\page.tsx', '..\\..\\src\\app');

    expect(routeModule).toBe('_________src_api_get');
    expect(notFound).toBe('__not_found');
    expect(layout).toBe('__dashboard_layoutLayout');
    expect(lazyPage).toBe("() => import('../../src/app/blog/[slug]/page')");

    helper.write();
    const source = readFileSync(outputFile, 'utf8');
    expect(source).toContain("import * as _________src_api_get from '../../../src/api/get';");
    expect(source).toContain("import * as __not_found from '../../../src/app/not-found';");
    expect(source).toContain("import __dashboard_layoutLayout from '../../src/app/dashboard/layout';");
    expect(source).not.toContain('\\');
  });
});
