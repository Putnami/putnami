import { generateTypeScriptClient, readOpenApiSource } from '@putnami/client/generator';
import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

// Generation cell of the cross-language interop matrix, provider side. A client
// is built, not committed (tooling/clientgen-extension ADR 0007): what this
// level owns is that the committed contract renders reproducibly, carries every
// declared operation, and that a contract break changes what is rendered.

const CONTRACT = join(import.meta.dir, '..', 'schema', 'openapi.json');

/** The feature this sample owns in putnami.features.json. */
const FEATURE = 'samples/ts-first-party-client-matrix';

/** The rendered closure of a contract, as a path → content map. */
function render(contract: ReturnType<typeof readOpenApiSource>): Record<string, string> {
  const files = generateTypeScriptClient(contract, { packageName: '@example/items-client' });
  return Object.fromEntries(files.map((file) => [file.path, file.content]));
}

function readContract() {
  return readOpenApiSource(readFileSync(CONTRACT, 'utf8'), { mode: 'firstParty' });
}

describe('generated client', () => {
  specTest(
    'renders every declared operation, and the same files on every run',
    {
      feature: FEATURE,
      requirement: 'generation-is-deterministic-and-a-break-is-detected',
      check: 'the-committed-contract-renders-reproducibly',
    },
    () => {
      // The same emitter each client project's client-generate runs, on the
      // same committed contract.
      const contract = readContract();
      const first = render(contract);
      expect(Object.keys(first).length).toBeGreaterThan(0);
      expect(render(readContract())).toEqual(first);

      // Every declared operation is rendered as a method a consumer can call.
      const rendered = Object.values(first).join('\n');
      for (const service of contract.services) {
        for (const method of service.methods) {
          expect(rendered).toContain(method.operationId);
        }
      }
    },
  );

  specTest(
    'detects a contract break instead of rendering through it',
    {
      feature: FEATURE,
      requirement: 'generation-is-deterministic-and-a-break-is-detected',
      check: 'a-removed-operation-is-detected-rather-than-rendered-through',
    },
    () => {
      // A removed operation is the break a consumer must not discover at run
      // time. Rendering the amputated contract drops that operation's client
      // and changes the index every consumer imports, so the client project
      // that builds from it fails to compile where it calls the removed method.
      const full = render(readContract());
      const amputated = readContract();
      for (const service of amputated.services) {
        service.methods = service.methods.filter((method) => method.path !== '/whoami');
      }
      amputated.services = amputated.services.filter((service) => service.methods.length > 0);
      const broken = render(amputated);

      expect(Object.keys(full).some((path) => path.endsWith('whoami-client.ts'))).toBe(true);
      expect(Object.keys(broken).some((path) => path.endsWith('whoami-client.ts'))).toBe(false);
      expect(broken['src/index.ts']).toBeDefined();
      expect(broken['src/index.ts']).not.toBe(full['src/index.ts']);
    },
  );
});
