import { describe, expect, test } from 'bun:test';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, realpathSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { resolveBiomeCli } from '../../src/generator/generated-file-canonicalizer';
import { generateProjectClients } from '../../src/generator/project-clients';

// One-operation OpenAPI document → a single service with one method.
const MINIMAL_SPEC = JSON.stringify({
  openapi: '3.0.3',
  info: { title: 'Items', version: '1.0.0' },
  paths: {
    '/items/{id}': {
      get: {
        operationId: 'getItem',
        parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }],
        responses: { '204': { description: 'No Content' } },
      },
    },
  },
});

const TS_CONFIG = JSON.stringify({
  thirdParty: true,
  targets: ['ts'],
  ts: { output: 'clients/ts', packageName: '@example/items-client' },
  go: { output: 'clients/go', modulePath: '', packageName: 'client', clientName: 'Client' },
});

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

/**
 * JSON.parse/stringify cannot round-trip the corpus's exact uint64 bounds: a token
 * comes back as the nearest double and no longer fits its declared width. Restore
 * every wide integer the source publishes, and fail loudly rather than hand a
 * fixture to the reader that silently lost the contract's precision.
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

const FIRST_PARTY_CONFIG = JSON.stringify({
  targets: ['ts'],
  ts: { output: 'clients/ts', packageName: '@example/widgets-client' },
  go: { output: 'clients/go', modulePath: '', packageName: 'client', clientName: 'Client' },
});
const GENERATE_BIN = join(import.meta.dir, '../../bin/generate.ts');
const DEFAULT_BIOME_CONFIG = JSON.stringify({
  ...JSON.parse(readFileSync(join(import.meta.dir, '../../../../../biome.json'), 'utf8')),
  vcs: { enabled: false },
});
const FORMATTER_TEST_TIMEOUT_MS = 60_000;

function sha256(content: string | Uint8Array): string {
  return createHash('sha256').update(content).digest('hex');
}

function setupProject(files: Record<string, string>): string {
  // Keep the config and file-walker identities equal on macOS (/var -> /private/var).
  const root = realpathSync(mkdtempSync(join(tmpdir(), 'cg-ts-')));
  for (const [rel, content] of Object.entries({ 'biome.json': DEFAULT_BIOME_CONFIG, ...files })) {
    const p = join(root, rel);
    mkdirSync(dirname(p), { recursive: true });
    writeFileSync(p, content);
  }
  return root;
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
if (args.some((arg) => arg.endsWith('clients/ts/src/items-client.ts'))) process.exit(23);
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

describe('generateProjectClients (TypeScript)', () => {
  specTest(
    'canonicalizes generated files with workspace overrides and EditorConfig',
    {
      feature: 'typescript/service-clients',
      requirement: 'generated-parity',
      check: 'canonicalizes-project-generation-with-effective-biome-config',
    },
    () => {
      const workspace = setupProject({
        'putnami.workspace.json': '{}',
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
              includes: ['providers/items/clients/ts/src/**/*.ts'],
              javascript: { formatter: { quoteStyle: 'double' } },
            },
          ],
        }),
        'providers/items/.gen/clientgen/config.json': TS_CONFIG,
        'providers/items/schema/openapi.json': MINIMAL_SPEC,
      });
      const root = join(workspace, 'providers/items');

      const result = generateProjectClients(root);
      expect(result.generated).toBe(true);
      expect(result.outputDir).toBe('clients/ts');
      for (const file of [
        'clients/ts/package.json',
        'clients/ts/tsconfig.json',
        'clients/ts/src/index.ts',
        'clients/ts/src/types.ts',
      ]) {
        expect(existsSync(join(root, file))).toBe(true);
      }

      const pkg = JSON.parse(readFileSync(join(root, 'clients/ts/package.json'), 'utf8'));
      expect(pkg.name).toBe('@example/items-client');
      expect(pkg.dependencies['@putnami/client']).toBeDefined();

      const clientPath = join(root, 'clients/ts/src/items-client.ts');
      const client = readFileSync(clientPath, 'utf8');
      expect(client).toContain('ItemsClient');
      expect(client).toContain('from "@putnami/client"');
      expect(client).toContain('\n    readonly serviceName');

      const before = Object.fromEntries(result.files.map((file) => [file, readFileSync(join(root, file), 'utf8')]));
      const biomeCli = resolveBiomeCli(root);
      for (const [phase, args] of [
        ['format', ['--write']],
        ['lint', ['--write', '--unsafe']],
      ] as const) {
        const lintResult = spawnSync(
          process.execPath,
          [biomeCli, phase, ...args, `--config-path=${join(workspace, 'biome.json')}`, 'providers/items'],
          { cwd: workspace, encoding: 'utf8' },
        );
        if (lintResult.status !== 0) throw new Error(`Biome ${phase} failed for the generated fixture`);
      }
      expect(Object.fromEntries(result.files.map((file) => [file, readFileSync(join(root, file), 'utf8')]))).toEqual(
        before,
      );
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  specTest(
    'selects the provider-local Biome package before the client fallback',
    {
      feature: 'typescript/service-clients',
      requirement: 'canonical-generated-manifest',
      check: 'the-provider-local-biome-package-wins-over-the-client-fallback',
    },
    () => {
      const root = setupProject({ 'package.json': JSON.stringify({ private: true }) });
      const packageRoot = join(root, 'node_modules/@biomejs/biome');
      const bin = join(packageRoot, 'bin/biome');
      mkdirSync(dirname(bin), { recursive: true });
      writeFileSync(
        join(packageRoot, 'package.json'),
        JSON.stringify({
          name: '@biomejs/biome',
          version: '99.0.0-provider',
          exports: { './bin/biome': './bin/biome' },
        }),
      );
      writeFileSync(bin, '// provider-local biome shim\n');

      expect(resolveBiomeCli(root)).toBe(bin);
    },
  );

  test(
    'emits a self-contained TS client package for a ts target',
    () => {
      const root = setupProject({
        '.gen/clientgen/config.json': TS_CONFIG,
        'schema/openapi.json': MINIMAL_SPEC,
      });

      const res = generateProjectClients(root);
      expect(res.generated).toBe(true);
      expect(res.outputDir).toBe('clients/ts');
      for (const f of [
        'clients/ts/package.json',
        'clients/ts/tsconfig.json',
        'clients/ts/src/index.ts',
        'clients/ts/src/types.ts',
      ]) {
        expect(existsSync(join(root, f))).toBe(true);
      }

      const pkg = JSON.parse(readFileSync(join(root, 'clients/ts/package.json'), 'utf8'));
      expect(pkg.name).toBe('@example/items-client');
      expect(pkg.dependencies['@putnami/client']).toBeDefined();

      const index = readFileSync(join(root, 'clients/ts/src/index.ts'), 'utf8');
      expect(index).toContain('ItemsClient');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'no-op when there is no clientgen contract',
    () => {
      const res = generateProjectClients(setupProject({ 'schema/openapi.json': MINIMAL_SPEC }));
      expect(res.generated).toBe(false);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'no-op when the ts target is disabled',
    () => {
      const root = setupProject({
        '.gen/clientgen/config.json': JSON.stringify({
          targets: ['go'],
          ts: { output: 'clients/ts', packageName: '@example/c' },
          go: { output: 'clients/go', modulePath: '', packageName: 'client', clientName: 'Client' },
        }),
        'schema/openapi.json': MINIMAL_SPEC,
      });
      const res = generateProjectClients(root);
      expect(res.generated).toBe(false);
      expect(existsSync(join(root, 'clients/ts'))).toBe(false);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'reads the spec from the .gen fallback (openapi output:false / Go provider)',
    () => {
      const root = setupProject({
        '.gen/clientgen/config.json': TS_CONFIG,
        '.gen/schema/openapi.json': MINIMAL_SPEC,
      });
      expect(generateProjectClients(root).generated).toBe(true);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'prefers the freshly built .gen contract over a stale committed copy',
    () => {
      const fresh = MINIMAL_SPEC.replaceAll('/items/{id}', '/fresh/{id}').replace('getItem', 'getFresh');
      const stale = MINIMAL_SPEC.replaceAll('/items/{id}', '/stale/{id}').replace('getItem', 'getStale');
      const root = setupProject({
        '.gen/clientgen/config.json': TS_CONFIG,
        '.gen/schema/openapi.json': fresh,
        'schema/openapi.json': stale,
      });

      generateProjectClients(root);
      const source = readFileSync(join(root, 'clients/ts/src/fresh-client.ts'), 'utf8');
      expect(source).toContain("'/fresh/{id}'");
      expect(existsSync(join(root, 'clients/ts/src/stale-client.ts'))).toBe(false);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  specTest(
    'writes a deterministic first-party inventory with actual generated symbols and hashes',
    {
      feature: 'typescript/service-clients',
      requirement: 'canonical-generated-manifest',
      check: 'manifest-hashes-the-canonical-biome-output',
    },
    () => {
      const source = firstPartySpec();
      const root = setupProject({
        '.gen/clientgen/config.json': FIRST_PARTY_CONFIG,
        '.gen/schema/openapi.json': source,
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

      const result = generateProjectClients(root);
      const manifestSource = readFileSync(join(root, 'clients/ts/client.putnami.json'), 'utf8');
      const manifest = JSON.parse(manifestSource);
      expect(result.files).toContain('clients/ts/client.putnami.json');
      expect(manifest).toMatchObject({
        protocolVersion: 1,
        generatedBy: '@putnami/clientgen',
        language: 'ts',
        service: { id: 'fixtures.widgets', audience: 'urn:putnami:fixtures:widgets' },
        binding: {
          importPath: '@example/widgets-client',
          clients: [
            { service: 'WidgetsService', clientSymbol: 'WidgetsClient', bindingSymbol: 'registerWidgetsClient' },
          ],
        },
        contractSha256: sha256(source),
        operations: [
          {
            operationId: 'getWidget',
            service: 'WidgetsService',
            methodSymbol: 'getWidget',
            stream: 'unary',
            transports: [{ protocol: 'rest-json', path: '/widgets/{id}', encoding: 'json' }],
          },
        ],
      });
      expect(manifest.files.map((file: { path: string }) => file.path)).toEqual([
        'package.json',
        'src/index.ts',
        'src/types.ts',
        'src/widgets-client.ts',
        'tsconfig.json',
      ]);
      for (const file of manifest.files) {
        expect(file.sha256).toBe(sha256(readFileSync(join(root, 'clients/ts', file.path))));
      }
      const generatedClient = readFileSync(join(root, 'clients/ts/src/widgets-client.ts'), 'utf8');
      expect(generatedClient).toContain("serviceId: 'fixtures.widgets'");
      expect(generatedClient).toContain('\n    contract: {');
      expect(generatedClient).not.toContain('const SERVICE_DESCRIPTOR = {"contract"');
      expect(generateProjectClients(root)).toEqual(result);
      expect(readFileSync(join(root, 'clients/ts/src/widgets-client.ts'), 'utf8')).toBe(generatedClient);
      expect(readFileSync(join(root, 'clients/ts/client.putnami.json'), 'utf8')).toBe(manifestSource);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  specTest(
    'pins the response-cache capability in the emitted module and declares the policy in the manifest',
    {
      feature: 'typescript/service-clients',
      requirement: 'response-cache',
      check: 'the-emitted-ts-client-pins-the-response-cache-capability-and-its-manifest-declares-the-policy',
    },
    () => {
      // The shared corpus declares fresh 5 s / stale 5 min on getWidget.
      const root = setupProject({
        '.gen/clientgen/config.json': FIRST_PARTY_CONFIG,
        '.gen/schema/openapi.json': firstPartySpec(),
      });
      generateProjectClients(root);
      const manifestPath = join(root, 'clients/ts/client.putnami.json');
      const manifestSource = readFileSync(manifestPath, 'utf8');
      const manifest = JSON.parse(manifestSource);
      expect(manifest.runtimeCapabilities).toEqual(['response-cache']);
      // The workspace formats every client.putnami.json expanded, the way the
      // Go emitter writes it (biome.json `expand: always`); the manifest must
      // already be in that form or the client project fails lint.
      expect(manifestSource).toContain('"runtimeCapabilities": [\n    "response-cache"\n  ]');
      expect(manifestSource).toMatch(/"keyFields": \[\n\s+"path\.id"\n\s+\]/);
      const formatResult = spawnSync(
        process.execPath,
        [resolveBiomeCli(root), 'format', '--write', `--config-path=${join(root, 'biome.json')}`, 'clients/ts'],
        { cwd: root, encoding: 'utf8' },
      );
      expect(formatResult.status).toBe(0);
      expect(readFileSync(manifestPath, 'utf8')).toBe(manifestSource);
      expect(manifest.operations[0].cache).toEqual({
        freshMs: 5000,
        staleMs: 300_000,
        maxEntries: 256,
        keyFields: ['path.id'],
        invalidationFields: ['id'],
      });
      // The policy keeps the Go emitter's field order too.
      expect(Object.keys(manifest.operations[0].cache)).toEqual([
        'freshMs',
        'staleMs',
        'maxEntries',
        'keyFields',
        'invalidationFields',
      ]);
      // The manifest keeps the Go emitter's field order: capabilities after the
      // operations, before the files.
      expect(Object.keys(manifest)).toEqual([
        'protocolVersion',
        'generatedBy',
        'language',
        'service',
        'binding',
        'contractSha256',
        'operations',
        'runtimeCapabilities',
        'files',
      ]);
      const generatedClient = readFileSync(join(root, 'clients/ts/src/widgets-client.ts'), 'utf8');
      expect(generatedClient).toContain('requireClientRuntimeCapabilities');
      expect(generatedClient).toContain("requireClientRuntimeCapabilities(['response-cache']);");
      expect(generatedClient).toContain('freshMs: 5000');
      // A cached method forwards the caller's bypass; its call options declare it.
      expect(generatedClient).toContain('withoutResponseCache: options?.withoutResponseCache,');
      const generatedTypes = readFileSync(join(root, 'clients/ts/src/types.ts'), 'utf8');
      expect(generatedTypes).toContain('withoutResponseCache?: boolean;');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'removes stale owned files while preserving unrelated output files',
    () => {
      const source = firstPartySpec();
      const root = setupProject({
        '.gen/clientgen/config.json': FIRST_PARTY_CONFIG,
        '.gen/schema/openapi.json': source,
      });
      generateProjectClients(root);
      const output = join(root, 'clients/ts');
      const manifestPath = join(output, 'client.putnami.json');
      const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
      writeFileSync(join(output, 'unrelated.txt'), 'keep me');
      writeFileSync(join(output, 'src/obsolete.ts'), '// generated old byte\n');
      manifest.files.push({ path: 'src/obsolete.ts', sha256: sha256('// generated old byte\n') });
      manifest.files.sort((left: { path: string }, right: { path: string }) => left.path.localeCompare(right.path));
      writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);

      generateProjectClients(root);
      expect(existsSync(join(output, 'src/obsolete.ts'))).toBe(false);
      expect(readFileSync(join(output, 'unrelated.txt'), 'utf8')).toBe('keep me');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'refuses to delete a stale generated path whose bytes no longer match its inventory',
    () => {
      const source = firstPartySpec();
      const root = setupProject({
        '.gen/clientgen/config.json': FIRST_PARTY_CONFIG,
        '.gen/schema/openapi.json': source,
      });
      generateProjectClients(root);
      const output = join(root, 'clients/ts');
      const manifestPath = join(output, 'client.putnami.json');
      const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
      writeFileSync(join(output, 'src/obsolete.ts'), '// locally edited\n');
      manifest.files.push({ path: 'src/obsolete.ts', sha256: sha256('// generated old byte\n') });
      manifest.files.sort((left: { path: string }, right: { path: string }) => left.path.localeCompare(right.path));
      writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);

      expect(() => generateProjectClients(root)).toThrow(/refusing to remove modified generated file/);
      expect(readFileSync(join(output, 'src/obsolete.ts'), 'utf8')).toBe('// locally edited\n');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'removes the previous first-party target when the provider has no operations left',
    () => {
      const source = firstPartySpec();
      const root = setupProject({
        '.gen/clientgen/config.json': FIRST_PARTY_CONFIG,
        '.gen/schema/openapi.json': source,
      });
      generateProjectClients(root);
      const output = join(root, 'clients/ts');
      writeFileSync(join(output, 'notes.md'), 'consumer-owned');
      const empty = JSON.parse(source);
      empty.paths = {};
      writeFileSync(join(root, '.gen/schema/openapi.json'), `${preserveWideIntegers(source, JSON.stringify(empty))}\n`);

      expect(generateProjectClients(root)).toEqual({ generated: false, outputDir: 'clients/ts', files: [] });
      expect(existsSync(join(output, 'client.putnami.json'))).toBe(false);
      expect(existsSync(join(output, 'src/widgets-client.ts'))).toBe(false);
      expect(readFileSync(join(output, 'notes.md'), 'utf8')).toBe('consumer-owned');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'throws when the ts target is enabled but no spec exists',
    () => {
      const root = setupProject({ '.gen/clientgen/config.json': TS_CONFIG });
      expect(() => generateProjectClients(root)).toThrow();
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'does not expose generated input when canonical formatting fails',
    () => {
      const root = setupProject({
        '.gen/clientgen/config.json': FIRST_PARTY_CONFIG,
        '.gen/schema/openapi.json': firstPartySpec(),
        'biome.json': '{"runtime-only-token":',
      });

      let failure: unknown;
      try {
        generateProjectClients(root);
      } catch (error) {
        failure = error;
      }
      expect(failure).toBeInstanceOf(Error);
      expect((failure as Error).message).toMatch(/^clientgen_format_failed: Biome format failed/);
      expect((failure as Error).message).not.toContain('runtime-only-token');
      expect(existsSync(join(root, 'clients/ts/src/widgets-client.ts'))).toBe(false);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'uses the same canonical renderer through the standalone shim without a PATH formatter',
    () => {
      const root = setupProject({
        '.gen/clientgen/config.json': FIRST_PARTY_CONFIG,
        '.gen/schema/openapi.json': firstPartySpec(),
        'putnami.workspace.json': '{}',
      });
      const formatWorkspace = setupProject({
        '.editorconfig': 'root = true\n[*]\nindent_style = space\nindent_size = 4\n',
        'biome.json': JSON.stringify({
          formatter: { useEditorconfig: true },
          javascript: { formatter: { quoteStyle: 'double' } },
          overrides: [
            {
              includes: ['providers/widgets/clients/ts/src/**/*.ts'],
              javascript: { formatter: { quoteStyle: 'single' } },
            },
          ],
        }),
        'putnami.workspace.json': '{}',
      });
      const formatRoot = join(formatWorkspace, 'providers/widgets');
      mkdirSync(formatRoot, { recursive: true });
      const result = spawnSync(process.execPath, [GENERATE_BIN, '--project', root, '--format-project', formatRoot], {
        encoding: 'utf8',
        env: { ...process.env, PATH: dirname(process.execPath), PUTNAMI_WORKSPACE_ROOT: root },
      });

      if (result.status !== 0) throw new Error(result.stderr || 'standalone generator failed without a diagnostic');
      expect(result.stderr).toBe('');
      const client = readFileSync(join(root, 'clients/ts/src/widgets-client.ts'), 'utf8');
      expect(client).toContain("serviceId: 'fixtures.widgets'");
      expect(client).toContain('\n    contract: {');
      const manifest = JSON.parse(readFileSync(join(root, 'clients/ts/client.putnami.json'), 'utf8'));
      for (const file of manifest.files) {
        expect(file.sha256).toBe(sha256(readFileSync(join(root, 'clients/ts', file.path))));
      }
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  // A provider serving "/.well-known/..." must not fail with exit 1 and no
  // message: the class name is not an identifier, so the shim must not
  // swallow the plain Error the emitter throws.
  test(
    'generates a first-party client for a /.well-known path through the standalone shim',
    () => {
      const fixture = JSON.parse(firstPartySpec());
      fixture.paths = { '/.well-known/widgets/{id}': fixture.paths['/widgets/{id}'] };
      // The canonical id a Go provider publishes for this path keeps the
      // dot; it must not reach the emitted type names verbatim
      // ("Get.wellKnownWidgets_IdResult"), or Biome refuses the whole client.
      fixture.paths['/.well-known/widgets/{id}'].get.operationId = 'get.well-known_Widgets_Id';
      const root = setupProject({
        '.gen/clientgen/config.json': FIRST_PARTY_CONFIG,
        '.gen/schema/openapi.json': `${preserveWideIntegers(firstPartySpec(), JSON.stringify(fixture, null, 2))}\n`,
        'putnami.workspace.json': '{}',
      });
      const result = spawnSync(process.execPath, [GENERATE_BIN, '--project', root], {
        encoding: 'utf8',
        env: { ...process.env, PUTNAMI_WORKSPACE_ROOT: root },
      });

      if (result.status !== 0) throw new Error(result.stderr || 'standalone generator failed without a diagnostic');
      expect(readFileSync(join(root, 'clients/ts/src/well-known-client.ts'), 'utf8')).toContain(
        'export class WellKnownClient',
      );
      const types = readFileSync(join(root, 'clients/ts/src/types.ts'), 'utf8');
      expect(types).toContain('GetWellKnownWidgetsId');
      expect(types).not.toContain('Get.well');
      // The canonical id stays the wire identity; only the derived names change.
      expect(readFileSync(join(root, 'clients/ts/src/well-known-client.ts'), 'utf8')).toContain(
        "operationId: 'get.well-known_Widgets_Id',",
      );
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test('the standalone shim prints an unexpected exception instead of exiting silently', () => {
    const root = setupProject({ '.gen/clientgen/config.json': '{"targets": [' });

    const result = spawnSync(process.execPath, [GENERATE_BIN, '--project', root], { encoding: 'utf8' });

    expect(result.status).toBe(1);
    expect(result.stdout).toBe('');
    expect(result.stderr).toMatch(/^SyntaxError: /);
    expect(result.stderr).toContain('project-clients.ts');
  });

  test('preserves existing client files when canonicalization fails', () => {
    const existing = {
      'clients/ts/package.json': '{"existing":true}\n',
      'clients/ts/src/items-client.ts': '// existing client\n',
      'clients/ts/src/types.ts': '// existing types\n',
    };
    const root = setupProject({
      '.gen/clientgen/config.json': TS_CONFIG,
      'schema/openapi.json': MINIMAL_SPEC,
      'biome.json': JSON.stringify({
        root: true,
        vcs: { enabled: false },
        formatter: { enabled: true },
        linter: { enabled: true, rules: { recommended: false } },
      }),
      ...existing,
    });
    installLateBiomeFailure(root, resolveBiomeCli(import.meta.dir));

    expect(() => generateProjectClients(root)).toThrow(
      /^clientgen_format_failed: Biome format failed for src\/items-client\.ts/,
    );
    for (const [file, content] of Object.entries(existing)) {
      expect(readFileSync(join(root, file), 'utf8')).toBe(content);
    }
  });

  test('prints only the sanitized formatter failure from the standalone CLI', () => {
    const configSecret = 'configuration-secret-do-not-echo';
    const sourceSecret = 'sourceSecretDoNotEcho';
    const root = setupProject({
      '.gen/clientgen/config.json': TS_CONFIG,
      'schema/openapi.json': MINIMAL_SPEC.replace('getItem', sourceSecret),
      'biome.json': `{"${configSecret}":`,
    });
    const bunInstallCache = join(root, '.bun-install-cache');
    mkdirSync(bunInstallCache);

    const result = spawnSync(process.execPath, [GENERATE_BIN, '--project', root], {
      encoding: 'utf8',
      env: { ...process.env, BUN_INSTALL_CACHE_DIR: bunInstallCache },
    });

    expect(result.status).toBe(1);
    expect(result.stdout).toBe('');
    // One sanitized line: Biome's own summary of what it refused, and none of
    // the code frames that quote the provider's configuration or its source.
    expect(result.stderr).toBe(
      'clientgen_format_failed: Biome format failed for src/types.ts (exit 1): ' +
        'Expected an array, an object, or a literal but instead found the end of the file. ' +
        'Biome exited because the configuration resulted in errors. Please fix them.\n',
    );
    expect(result.stderr).not.toContain(configSecret);
    expect(result.stderr).not.toContain(sourceSecret);
    expect(readdirSync(bunInstallCache)).toEqual([]);
  });

  test('rejects a ts.output that escapes the project root', () => {
    const root = setupProject({
      '.gen/clientgen/config.json': JSON.stringify({
        targets: ['ts'],
        ts: { output: '../escape', packageName: '@example/c' },
        go: { output: 'clients/go', modulePath: '', packageName: 'client', clientName: 'Client' },
      }),
      'schema/openapi.json': MINIMAL_SPEC,
    });
    expect(() => generateProjectClients(root)).toThrow();
  });
});
