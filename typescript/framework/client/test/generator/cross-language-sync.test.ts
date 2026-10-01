import { describe, expect } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { getWorkspaceRoot, joinPath } from '@putnami/utils';
import { generateProjectClients } from '../../src/generator/project-clients';

// Phase 3 / Task 4 — cross-language integration coverage (the TS half).
//
// The Go `service-to-service` provider declares targets:['go','ts'] and commits
// its cross-language TypeScript client. Here we regenerate that client from the
// provider's committed OpenAPI spec and assert the emitted source matches the
// committed files. A Go-side API change that lands without re-running
// `putnami clientgen` must fail this test rather than ship a stale TS client.
//
// (The Go half — a TS provider's committed clients/go — is guarded by a sibling
// Go test in go/framework/api: clientgen_crosslang_test.go.)
describe('cross-language clients integration (Go provider -> TS client)', () => {
  specTest(
    'committed clients/ts is in sync with the Go provider spec',
    {
      feature: 'typescript/service-clients',
      requirement: 'generated-parity',
      check: 'the-committed-typescript-client-is-in-sync-with-the-provider-spec',
    },
    () => {
      const ws = getWorkspaceRoot();
      const sampleRoot = joinPath(ws, 'go', 'samples', 'service-to-service');
      const committedTs = join(sampleRoot, 'clients', 'ts');

      if (!existsSync(committedTs)) {
        throw new Error(`expected committed cross-language client at ${committedTs}`);
      }
      const providerSpec = readFileSync(join(sampleRoot, 'schema', 'openapi.json'));
      // Provider generation refuses an unmarked spec by default; `thirdParty` is
      // the declared opt-in for a contract that carries no `x-putnami-client`.
      // The Go sample publishes the marker, so the flag is a no-op here — it
      // is computed rather than hard-coded so this guard holds for an
      // unmarked provider too.
      const firstParty = providerSpec.includes(Buffer.from('"x-putnami-client"'));
      // The producer table is READ from the provider's own document instead of
      // being restated here: a hard-coded operation list silently goes stale the
      // moment the provider declares a new route, and this guard would then
      // compare the wrong contract.
      const document = JSON.parse(providerSpec.toString('utf8')) as {
        paths: Record<string, Record<string, unknown>>;
      };
      const designOperations = Object.entries(document.paths).flatMap(([path, item]) =>
        Object.keys(item)
          .filter((method) => ['get', 'put', 'post', 'delete', 'patch', 'head', 'options'].includes(method))
          .map((method) => ({
            method: method.toUpperCase(),
            path,
            producerProject: 'go.putnami.dev/examples/service-to-service',
            producerFeature: 'items/manage',
          })),
      );

      // Mirror the provider's generate inputs (committed spec + the clientgen
      // contract its build emits) in a throwaway project, then regenerate.
      const tmp = mkdtempSync(join(tmpdir(), 'cg-xlang-ts-'));
      try {
        mkdirSync(join(tmp, 'schema'), { recursive: true });
        writeFileSync(join(tmp, 'schema', 'openapi.json'), providerSpec);
        mkdirSync(join(tmp, '.gen', 'clientgen'), { recursive: true });
        writeFileSync(
          join(tmp, '.gen', 'clientgen', 'config.json'),
          JSON.stringify({
            ...(firstParty ? {} : { thirdParty: true }),
            targets: ['go', 'ts'],
            ts: { output: 'clients/ts', packageName: '@example/go-items-client' },
            go: { output: 'clients/go', modulePath: '', packageName: 'itemsclient', clientName: 'ItemsClient' },
            design: { operations: designOperations },
          }),
        );

        const res = generateProjectClients(tmp, { formatProjectRoot: sampleRoot });
        expect(res.generated).toBe(true);

        // Compare the client source + package.json. tsconfig.json is excluded: its
        // `extends` is computed relative to the output dir's depth, which differs
        // between the temp project and the committed sample.
        const srcFiles = readdirSync(join(committedTs, 'src')).sort();
        expect(srcFiles.length).toBeGreaterThan(0);

        // Every emitted byte is compared exactly. The transitional spec-hash
        // substitution this test used to apply is gone: the provider is
        // first-party, the strict emitter runs, and the committed bytes are
        // what a fresh render produces.
        const emittedFiles = readdirSync(join(tmp, 'clients', 'ts', 'src')).sort();
        expect(emittedFiles).toEqual(srcFiles);
        for (const f of srcFiles) {
          const got = readFileSync(join(tmp, 'clients', 'ts', 'src', f), 'utf8');
          const committed = readFileSync(join(committedTs, 'src', f), 'utf8');
          expect(got, `clients/ts/src/${f} drifted — run \`putnami clientgen\` and commit`).toBe(committed);
        }
        expect(readFileSync(join(tmp, 'clients', 'ts', 'package.json'), 'utf8')).toBe(
          readFileSync(join(committedTs, 'package.json'), 'utf8'),
        );
      } finally {
        rmSync(tmp, { force: true, recursive: true });
      }
    },
    60_000,
  );
});
