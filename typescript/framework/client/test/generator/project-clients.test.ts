import { describe, expect, test } from 'bun:test';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { resolveBiomeCli } from '../../src/generator/generated-file-canonicalizer';
import { generateClient } from '../../src/generator/project-clients';

const GENERATE_BIN = join(import.meta.dir, '../../bin/generate.ts');
const DEFAULT_BIOME_CONFIG = JSON.stringify({
  ...JSON.parse(readFileSync(join(import.meta.dir, '../../../../../biome.json'), 'utf8')),
  vcs: { enabled: false },
});
const FORMATTER_TEST_TIMEOUT_MS = 60_000;

/** The client project's authored files, which generation never touches. */
const AUTHORED = {
  'clients/widgets/package.json': '{"name":"@example/widgets-client","private":true}\n',
  'clients/widgets/tsconfig.json': '{"extends":"../../tsconfig.base.json"}\n',
  'clients/widgets/putnami.json': '{"name":"@example/widgets-client","generatedFrom":{"service":"fixtures.widgets"}}\n',
};

/** One first-party operation from the shared corpus, served over REST only. */
function firstPartySpec(): string {
  const source = readFileSync(
    join(import.meta.dir, '../../../../../protocols/clientcontract/fixtures/openapi/valid/full.openapi.json'),
    'utf8',
  );
  const fixture = JSON.parse(source);
  fixture.paths = { '/widgets/{id}': fixture.paths['/widgets/{id}'] };
  fixture.paths['/widgets/{id}'].get['x-putnami-client'].transports = [
    fixture.paths['/widgets/{id}'].get['x-putnami-client'].transports[1],
  ];
  fixture['x-putnami-client'].protobuf = undefined;
  return `${preserveWideIntegers(source, JSON.stringify(fixture, null, 2))}\n`;
}

/** The first-party spec with its operation attributed to a producer. */
function attributedSpec(): string {
  const source = firstPartySpec();
  const fixture = JSON.parse(source);
  fixture.paths['/widgets/{id}'].get['x-putnami-client'].producer = {
    project: '@example/widgets',
    feature: 'widgets/read',
  };
  return `${preserveWideIntegers(source, JSON.stringify(fixture, null, 2))}\n`;
}

/** The first-party spec with every operation removed. */
function emptySpec(): string {
  const source = firstPartySpec();
  const fixture = JSON.parse(source);
  fixture.paths = {};
  return `${preserveWideIntegers(source, JSON.stringify(fixture, null, 2))}\n`;
}

/**
 * JSON.parse/stringify cannot round-trip the corpus's exact uint64 bounds: a token
 * past 2^53 comes back rounded. Restore the source's own digits for every wide
 * integer the source publishes, and fail loudly rather than hand a fixture to the
 * reader that silently lost the contract's precision.
 */
function preserveWideIntegers(source: string, text: string): string {
  let restored = text;
  for (const token of source.match(/(?<=:\s)-?\d{16,}(?=[,\s}])/g) ?? []) {
    const rounded = String(Number(token));
    if (rounded === token) continue;
    if (!restored.includes(rounded)) throw new Error(`wide integer ${token} is absent from the fixture`);
    restored = restored.replaceAll(rounded, token);
  }
  return restored;
}

interface Fixture {
  workspace: string;
  contract: string;
  client: string;
}

/**
 * A workspace holding a provider's committed contract and a client project with
 * its authored files. `files` are workspace-relative and win over the defaults.
 */
function setupWorkspace(files: Record<string, string> = {}): Fixture {
  // Keep the config and file-walker identities equal on macOS (/var -> /private/var).
  const workspace = realpathSync(mkdtempSync(join(tmpdir(), 'cg-ts-')));
  for (const [rel, content] of Object.entries({
    'putnami.workspace.json': '{"name":"w"}\n',
    'biome.json': DEFAULT_BIOME_CONFIG,
    'provider/schema/openapi.json': firstPartySpec(),
    ...AUTHORED,
    ...files,
  })) {
    const path = join(workspace, rel);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, content);
  }
  return {
    workspace,
    contract: join(workspace, 'provider/schema/openapi.json'),
    client: join(workspace, 'clients/widgets'),
  };
}

function generate(fixture: Fixture) {
  return generateClient({
    contractPath: fixture.contract,
    outputDir: fixture.client,
    packageName: '@example/widgets-client',
  });
}

function runBin(args: string[], env: Record<string, string | undefined> = process.env) {
  return spawnSync(process.execPath, [GENERATE_BIN, ...args], { encoding: 'utf8', env });
}

function binArgs(fixture: Fixture): string[] {
  return ['--contract', fixture.contract, '--output', fixture.client, '--package-name', '@example/widgets-client'];
}

/** Every file under `dir`, relative and sorted. */
function listFiles(dir: string, prefix = ''): string[] {
  return readdirSync(dir, { withFileTypes: true })
    .flatMap((entry) =>
      entry.isDirectory() ? listFiles(join(dir, entry.name), `${prefix}${entry.name}/`) : [`${prefix}${entry.name}`],
    )
    .sort();
}

// Inject a process failure only after the earlier file has used the real Biome.
function installLateBiomeFailure(projectRoot: string, actualBiomeCli: string): void {
  const packageRoot = join(projectRoot, 'node_modules/@biomejs/biome');
  const biomeCli = join(packageRoot, 'bin/biome');
  mkdirSync(dirname(biomeCli), { recursive: true });
  writeFileSync(
    join(packageRoot, 'package.json'),
    JSON.stringify({
      name: '@biomejs/biome',
      version: '99.0.0-late-failure-fixture',
      exports: { './bin/biome': './bin/biome' },
    }),
  );
  writeFileSync(
    biomeCli,
    `const { spawnSync } = require('node:child_process');
const { readFileSync } = require('node:fs');
const args = process.argv.slice(2);
if (args.some((arg) => arg.endsWith('src/widgets-client.ts'))) process.exit(23);
const result = spawnSync(process.execPath, [${JSON.stringify(actualBiomeCli)}, ...args], {
  encoding: 'utf8',
  env: process.env,
  input: readFileSync(0),
});
if (result.stdout) process.stdout.write(result.stdout);
if (result.stderr) process.stderr.write(result.stderr);
process.exit(result.status ?? 1);
`,
  );
}

describe('generateClient (TypeScript)', () => {
  specTest(
    'writes only the src closure and no manifest, package.json or tsconfig.json',
    {
      feature: 'tooling/cross-language-rest-clients',
      requirement: 'no-committed-client',
      check: 'the-typescript-emitter-writes-no-manifest',
    },
    () => {
      const fixture = setupWorkspace();
      // A file a previous render owned, which this contract no longer emits.
      mkdirSync(join(fixture.client, 'src'), { recursive: true });
      writeFileSync(join(fixture.client, 'src/obsolete-client.ts'), '// stale\n');

      const result = generate(fixture);

      expect(result).toEqual({
        generated: true,
        files: ['src/types.ts', 'src/widgets-client.ts', 'src/index.ts'],
      });
      expect(listFiles(fixture.client)).toEqual([
        'package.json',
        'putnami.json',
        'src/index.ts',
        'src/types.ts',
        'src/widgets-client.ts',
        'tsconfig.json',
      ]);
      for (const [rel, content] of Object.entries(AUTHORED)) {
        expect(readFileSync(join(fixture.workspace, rel), 'utf8')).toBe(content);
      }
      const client = readFileSync(join(fixture.client, 'src/widgets-client.ts'), 'utf8');
      expect(client).toContain("serviceId: 'fixtures.widgets'");
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  specTest(
    'renders the same bytes as a fresh workspace lint, with overrides and EditorConfig',
    {
      feature: 'typescript/service-clients',
      requirement: 'generated-parity',
      check: 'canonicalizes-project-generation-with-effective-biome-config',
    },
    () => {
      const fixture = setupWorkspace({
        '.editorconfig': 'root = true\n[*]\nindent_style = space\nindent_size = 4\ninsert_final_newline = true\n',
        'biome.base.json': JSON.stringify({
          formatter: { enabled: true, useEditorconfig: true },
          linter: {
            enabled: true,
            rules: { recommended: false, style: { useConsistentArrayType: 'error' } },
          },
          javascript: { formatter: { quoteStyle: 'single' } },
        }),
        'biome.json': JSON.stringify({
          root: true,
          extends: ['./biome.base.json'],
          vcs: { enabled: false },
          overrides: [
            {
              includes: ['clients/widgets/src/**/*.ts'],
              javascript: { formatter: { quoteStyle: 'double' } },
            },
          ],
        }),
      });

      const result = generate(fixture);
      const client = readFileSync(join(fixture.client, 'src/widgets-client.ts'), 'utf8');
      expect(client).toContain('from "@putnami/client"');
      expect(client).toContain('\n    readonly serviceName');

      const before = Object.fromEntries(
        result.files.map((file) => [file, readFileSync(join(fixture.client, file), 'utf8')]),
      );
      const biomeCli = resolveBiomeCli(fixture.client);
      for (const [phase, args] of [
        ['format', ['--write']],
        ['lint', ['--write', '--unsafe']],
      ] as const) {
        const lintResult = spawnSync(
          process.execPath,
          [biomeCli, phase, ...args, `--config-path=${join(fixture.workspace, 'biome.json')}`, 'clients/widgets/src'],
          { cwd: fixture.workspace, encoding: 'utf8' },
        );
        if (lintResult.status !== 0) throw new Error(`Biome ${phase} failed for the generated fixture`);
      }
      expect(
        Object.fromEntries(result.files.map((file) => [file, readFileSync(join(fixture.client, file), 'utf8')])),
      ).toEqual(before);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  specTest(
    'renders identical bytes on a second run, already in their canonical form',
    {
      feature: 'typescript/service-clients',
      requirement: 'canonical-generated-sources',
      check: 'the-emitted-sources-are-the-canonical-biome-output',
    },
    () => {
      const fixture = setupWorkspace({
        '.editorconfig': 'root = true\n[*]\nindent_style = space\nindent_size = 4\nend_of_line = lf\n',
        'biome.json': JSON.stringify({
          formatter: { enabled: true, useEditorconfig: true },
          linter: {
            enabled: true,
            rules: {
              correctness: { noUnusedImports: 'error' },
              style: { useConsistentArrayType: { level: 'warn', options: { syntax: 'shorthand' } } },
            },
          },
          javascript: { formatter: { quoteStyle: 'single' } },
        }),
      });

      const first = generate(fixture);
      const rendered = Object.fromEntries(
        first.files.map((file) => [file, readFileSync(join(fixture.client, file), 'utf8')]),
      );
      const client = rendered['src/widgets-client.ts'];
      expect(client).toContain('\n    contract: {');
      expect(client).not.toContain('const SERVICE_DESCRIPTOR = {"contract"');

      expect(generate(fixture)).toEqual(first);
      expect(
        Object.fromEntries(first.files.map((file) => [file, readFileSync(join(fixture.client, file), 'utf8')])),
      ).toEqual(rendered);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  specTest(
    "selects the client project's Biome package before the emitter's fallback",
    {
      feature: 'typescript/service-clients',
      requirement: 'canonical-generated-sources',
      check: 'the-format-projects-biome-package-wins-over-the-emitters-fallback',
    },
    () => {
      const fixture = setupWorkspace();
      const packageRoot = join(fixture.client, 'node_modules/@biomejs/biome');
      const bin = join(packageRoot, 'bin/biome');
      mkdirSync(dirname(bin), { recursive: true });
      writeFileSync(
        join(packageRoot, 'package.json'),
        JSON.stringify({
          name: '@biomejs/biome',
          version: '99.0.0-client',
          exports: { './bin/biome': './bin/biome' },
        }),
      );
      writeFileSync(bin, '// client-local biome shim\n');

      expect(resolveBiomeCli(fixture.client)).toBe(bin);
    },
  );

  specTest(
    'pins the response-cache capability in the emitted module',
    {
      feature: 'typescript/service-clients',
      requirement: 'response-cache',
      check: 'the-emitted-ts-client-pins-the-response-cache-capability',
    },
    () => {
      // The shared corpus declares fresh 5 s / stale 5 min on getWidget.
      const fixture = setupWorkspace();
      generate(fixture);
      const client = readFileSync(join(fixture.client, 'src/widgets-client.ts'), 'utf8');
      expect(client).toContain("requireClientRuntimeCapabilities(['response-cache']);");
      expect(client).toContain('freshMs: 5000');
      // A cached method forwards the caller's bypass; its call options declare it.
      expect(client).toContain('withoutResponseCache: options?.withoutResponseCache,');
      expect(readFileSync(join(fixture.client, 'src/types.ts'), 'utf8')).toContain('withoutResponseCache?: boolean;');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    "carries the contract's producer attribution into the design declaration",
    () => {
      const fixture = setupWorkspace({ 'provider/schema/openapi.json': attributedSpec() });
      generate(fixture);
      const client = readFileSync(join(fixture.client, 'src/widgets-client.ts'), 'utf8');
      expect(client).toContain("producerProject: '@example/widgets'");
      expect(client).toContain("producerFeature: 'widgets/read'");
      expect(client).toContain('design: SERVICE_DESIGN,');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'removes src and keeps the authored files when the contract declares no operation',
    () => {
      const fixture = setupWorkspace();
      generate(fixture);
      writeFileSync(fixture.contract, emptySpec());

      expect(generate(fixture)).toEqual({ generated: false, files: [] });
      expect(existsSync(join(fixture.client, 'src'))).toBe(false);
      expect(listFiles(fixture.client)).toEqual(['package.json', 'putnami.json', 'tsconfig.json']);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test('refuses a contract without a document-level x-putnami-client', () => {
    const fixture = setupWorkspace();
    const unmarked = JSON.parse(readFileSync(fixture.contract, 'utf8'));
    unmarked['x-putnami-client'] = undefined;
    for (const operation of Object.values(unmarked.paths as Record<string, Record<string, Record<string, unknown>>>))
      for (const item of Object.values(operation)) item['x-putnami-client'] = undefined;
    writeFileSync(fixture.contract, JSON.stringify(unmarked));

    expect(() => generate(fixture)).toThrow(/x-putnami-client/);
    expect(existsSync(join(fixture.client, 'src'))).toBe(false);
  });

  specTest(
    'preserves the existing client when canonical formatting fails',
    {
      feature: 'typescript/service-clients',
      requirement: 'generated-parity',
      check: 'a-formatter-failure-preserves-the-existing-client',
    },
    () => {
      const existing = {
        'clients/widgets/src/widgets-client.ts': '// existing client\n',
        'clients/widgets/src/types.ts': '// existing types\n',
      };
      const fixture = setupWorkspace({
        'biome.json': JSON.stringify({
          root: true,
          vcs: { enabled: false },
          formatter: { enabled: true },
          linter: { enabled: true, rules: { recommended: false } },
        }),
        ...existing,
      });
      installLateBiomeFailure(fixture.client, resolveBiomeCli(import.meta.dir));

      expect(() => generate(fixture)).toThrow(
        /^clientgen_format_failed: Biome format failed for src\/widgets-client\.ts/,
      );
      for (const [file, content] of Object.entries(existing)) {
        expect(readFileSync(join(fixture.workspace, file), 'utf8')).toBe(content);
      }
      expect(existsSync(join(fixture.client, 'src/index.ts'))).toBe(false);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );
});

describe('putnami-client-generate', () => {
  test(
    'formats with --format-project at the same relative paths, with no PATH formatter',
    () => {
      const fixture = setupWorkspace();
      // The real client project lives in another workspace whose Biome
      // configuration quotes the client's sources with single quotes only.
      const real = setupWorkspace({
        '.editorconfig': 'root = true\n[*]\nindent_style = space\nindent_size = 4\n',
        'biome.json': JSON.stringify({
          formatter: { useEditorconfig: true },
          javascript: { formatter: { quoteStyle: 'double' } },
          overrides: [
            {
              includes: ['clients/widgets/src/**/*.ts'],
              javascript: { formatter: { quoteStyle: 'single' } },
            },
          ],
        }),
      });

      const result = runBin([...binArgs(fixture), '--format-project', real.client], {
        ...process.env,
        PATH: dirname(process.execPath),
      });

      if (result.status !== 0) throw new Error(result.stderr || 'the emitter failed without a diagnostic');
      expect(result.stderr).toBe('');
      const client = readFileSync(join(fixture.client, 'src/widgets-client.ts'), 'utf8');
      expect(client).toContain("serviceId: 'fixtures.widgets'");
      expect(client).toContain('\n    contract: {');
      expect(existsSync(join(real.client, 'src'))).toBe(false);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  // A provider serving "/.well-known/..." must not fail with exit 1 and no
  // message: the class name is not an identifier, so the emitter must not
  // swallow the plain Error the generator throws.
  test(
    'generates a first-party client for a /.well-known path',
    () => {
      const spec = JSON.parse(firstPartySpec());
      spec.paths = { '/.well-known/widgets/{id}': spec.paths['/widgets/{id}'] };
      // The canonical id a Go provider publishes for this path keeps the
      // dot; it must not reach the emitted type names verbatim
      // ("Get.wellKnownWidgets_IdResult"), or Biome refuses the whole client.
      spec.paths['/.well-known/widgets/{id}'].get.operationId = 'get.well-known_Widgets_Id';
      const fixture = setupWorkspace({
        'provider/schema/openapi.json': `${preserveWideIntegers(firstPartySpec(), JSON.stringify(spec, null, 2))}\n`,
      });

      const result = runBin(binArgs(fixture));

      if (result.status !== 0) throw new Error(result.stderr || 'the emitter failed without a diagnostic');
      const client = readFileSync(join(fixture.client, 'src/well-known-client.ts'), 'utf8');
      expect(client).toContain('export class WellKnownClient');
      const types = readFileSync(join(fixture.client, 'src/types.ts'), 'utf8');
      expect(types).toContain('GetWellKnownWidgetsId');
      expect(types).not.toContain('Get.well');
      // The canonical id stays the wire identity; only the derived names change.
      expect(client).toContain("operationId: 'get.well-known_Widgets_Id',");
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test('refuses a missing, unknown or repeated flag with the usage line', () => {
    const fixture = setupWorkspace();
    for (const [args, message] of [
      [['--contract', fixture.contract, '--output', fixture.client], '--package-name is required'],
      [[...binArgs(fixture), '--project', fixture.client], 'unknown argument "--project"'],
      [[...binArgs(fixture), '--output', fixture.client], '--output is given twice'],
      [['--contract'], '--contract needs a value'],
    ] as const) {
      const result = runBin([...args]);
      expect(result.status).toBe(1);
      expect(result.stdout).toBe('');
      expect(result.stderr).toContain(message);
      expect(result.stderr).toContain('usage: putnami-client-generate --contract');
    }
    expect(existsSync(join(fixture.client, 'src'))).toBe(false);
  });

  test('exits 0 and removes src for a first-party contract with no operation', () => {
    const fixture = setupWorkspace({
      'provider/schema/openapi.json': emptySpec(),
      'clients/widgets/src/widgets-client.ts': '// previous render\n',
    });

    const result = runBin(binArgs(fixture));

    expect(result.status).toBe(0);
    expect(result.stderr).toBe('');
    expect(existsSync(join(fixture.client, 'src'))).toBe(false);
  });

  test('prints an unexpected exception instead of exiting silently', () => {
    const fixture = setupWorkspace({ 'provider/schema/openapi.json': '{"openapi": [' });

    const result = runBin(binArgs(fixture));

    expect(result.status).toBe(1);
    expect(result.stdout).toBe('');
    expect(result.stderr).not.toBe('');
  });

  test('prints only the sanitized formatter failure', () => {
    const configSecret = 'configuration-secret-do-not-echo';
    const fixture = setupWorkspace({ 'biome.json': `{"${configSecret}":` });
    const bunInstallCache = join(fixture.workspace, '.bun-install-cache');
    mkdirSync(bunInstallCache);

    const result = runBin(binArgs(fixture), { ...process.env, BUN_INSTALL_CACHE_DIR: bunInstallCache });

    expect(result.status).toBe(1);
    expect(result.stdout).toBe('');
    // One sanitized line: Biome's own summary of what it refused, and none of
    // the code frames that quote the configuration or the generated source.
    expect(result.stderr).toMatch(/^clientgen_format_failed: Biome format failed for src\/types\.ts \(exit 1\): /);
    expect(result.stderr.trimEnd().split('\n')).toHaveLength(1);
    expect(result.stderr).not.toContain(configSecret);
    expect(readdirSync(bunInstallCache)).toEqual([]);
  });
});
