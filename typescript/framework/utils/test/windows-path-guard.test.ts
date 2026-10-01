import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { Glob } from 'bun';

/**
 * A static guard over the product sources of every framework package, for
 * three Windows rules that tests on Linux and macOS cannot observe:
 *
 * - A native path is tested with `isAbsolutePath`, `dirname` or the host
 *   separator, never against `'/'`. The guard reads a camelCase name ending in
 *   `Path`, `Dir`, `File`, `Root` or `Directory` as a native path. A name that
 *   holds a URL path or a slash-separated wire path goes in
 *   `SLASH_FORM_NAMES`. A bare `path` is out of reach: most are URL paths.
 * - A rename goes through `@putnami/runtime/robustio`, which waits out the
 *   moment a Windows reader holds the target open. `@putnami/spectest` cannot
 *   depend on the runtime; its rename is content-addressed and tolerates a
 *   failure.
 * - Only `getProjectRoot` reads `PWD`, which Windows shells do not maintain.
 */

/** Identifiers that hold a URL path or a slash-separated wire path, never a native one. */
const SLASH_FORM_NAMES = new Set(['pageEvidencePath', 'remainingPath', 'routePath', 'urlPath']);

/** Files that own a rule's exception, relative to `typescript/framework`. */
const RENAME_OWNERS = new Set(['runtime/src/robustio/index.ts', 'spectest/src/index.ts']);
const PWD_OWNERS = new Set(['utils/src/server/workspace.utils.ts']);

const NATIVE_PATH_AGAINST_SLASH =
  /\b([A-Za-z_$][\w$]*[a-z0-9](?:Path|Dir|File|Root|Directory))\)?\.(?:startsWith|endsWith|lastIndexOf|indexOf|split|includes)\(\s*['"`]\/['"`]/g;
const RENAME_IMPORT = /import\s*\{[^}]*\brename(?:Sync)?\b[^}]*\}\s*from\s*['"](?:node:)?fs(?:\/promises)?['"]/;
const RENAME_MEMBER = /\b(?:fs|fsp|fsPromises|promises)\.rename(?:Sync)?\(/;
const PWD_READ = /\benv(?:\.PWD\b|\[\s*['"]PWD['"]\s*\])/;
/** A quoted `/`: NATIVE_PATH_AGAINST_SLASH cannot match a text without one. */
const SLASH_LITERAL = /['"`]\/['"`]/;

const FRAMEWORK_ROOT = resolve(import.meta.dir, '..', '..');

/** The rule violations in `text`, the content of `file`, one `file:line: rule` entry each. */
function violations(file: string, text: string): string[] {
  const found: string[] = [];
  const lines = text.split('\n');
  const importText = text.replace(/\s+/g, ' ');
  if (!RENAME_OWNERS.has(file) && RENAME_IMPORT.test(importText)) {
    found.push(`${file}: imports rename from node:fs; use robustRename from @putnami/runtime/robustio`);
  }
  for (const [index, line] of lines.entries()) {
    const at = `${file}:${index + 1}`;
    for (const match of line.matchAll(NATIVE_PATH_AGAINST_SLASH)) {
      const name = match[1] ?? '';
      if (!SLASH_FORM_NAMES.has(name)) {
        found.push(`${at}: tests native path ${name} against '/'; use isAbsolutePath, dirname or sep`);
      }
    }
    if (!RENAME_OWNERS.has(file) && RENAME_MEMBER.test(line)) {
      found.push(`${at}: calls rename from node:fs; use robustRename from @putnami/runtime/robustio`);
    }
    if (!PWD_OWNERS.has(file) && PWD_READ.test(line)) {
      found.push(`${at}: reads PWD; use getProjectRoot`);
    }
  }
  return found;
}

/**
 * Whether `text` holds a literal that one of the rules needs: a quoted `/`,
 * `rename` or `PWD`. Most files hold none, and they skip the line scan.
 */
function mayViolate(text: string): boolean {
  return SLASH_LITERAL.test(text) || text.includes('rename') || text.includes('PWD');
}

/** The rule violations in the product source `file`, read from disk. */
function sourceViolations(file: string): string[] {
  const text = readFileSync(join(FRAMEWORK_ROOT, file), 'utf8');
  return mayViolate(text) ? violations(file, text) : [];
}

/** Every product source file of the framework packages, relative to `typescript/framework`. */
function productSources(): string[] {
  const glob = new Glob('*/{src,bin}/**/*.{ts,tsx,mts,mjs}');
  return [...glob.scanSync({ cwd: FRAMEWORK_ROOT })]
    .map((file) => file.replaceAll('\\', '/'))
    .filter((file) => !file.includes('/node_modules/') && !file.includes('.gen.'))
    .sort();
}

const PRODUCT_SOURCES = productSources();

/**
 * Reading every product source is the part of the guard that grows with the
 * framework, and a host's first read of a file is the slow one. On the
 * Windows QA VM (e2-standard-4, Defender real-time scanning on) the first
 * reads of 750 files took 13.2 s, 17.6 ms a file; a warm run took 0.25 s. The
 * budget is 40 ms a file, over twice that worst measured cost, and never less
 * than bun's default 5 s.
 */
const READ_BUDGET_MS = Math.max(5000, PRODUCT_SOURCES.length * 40);

describe('Windows path guard', () => {
  it(
    'finds no violation in the framework product sources',
    () => {
      expect(PRODUCT_SOURCES.length).toBeGreaterThan(100);
      expect(PRODUCT_SOURCES.flatMap(sourceViolations)).toEqual([]);
    },
    READ_BUDGET_MS,
  );

  it('reports each rule', () => {
    const source = [
      "import { mkdir, rename } from 'node:fs/promises';",
      "const importPath = this.reactApplicationPath.startsWith('/') ? a : b;",
      "const dir = fullPath.slice(0, fullPath.lastIndexOf('/'));",
      "if (routePath.startsWith('/')) return routePath;",
      'fs.renameSync(from, to);',
      "const cwd = process.env['PWD'];",
    ].join('\n');

    expect(violations('web/src/x.ts', source)).toEqual([
      'web/src/x.ts: imports rename from node:fs; use robustRename from @putnami/runtime/robustio',
      "web/src/x.ts:2: tests native path reactApplicationPath against '/'; use isAbsolutePath, dirname or sep",
      "web/src/x.ts:3: tests native path fullPath against '/'; use isAbsolutePath, dirname or sep",
      'web/src/x.ts:5: calls rename from node:fs; use robustRename from @putnami/runtime/robustio',
      'web/src/x.ts:6: reads PWD; use getProjectRoot',
    ]);
    // Each line breaks a rule, so none may be skipped before the line scan.
    for (const line of source.split('\n')) {
      expect(mayViolate(line)).toBe(true);
    }
  });

  it('keeps each exception to the file that owns it', () => {
    expect(violations('runtime/src/robustio/index.ts', "import { rename, rm } from 'node:fs/promises';")).toEqual([]);
    expect(violations('utils/src/server/workspace.utils.ts', "const pwd = env['PWD'];")).toEqual([]);
    expect(violations('storage/src/x.ts', "import {\n  rename,\n} from 'node:fs/promises';")).toHaveLength(1);
  });
});
