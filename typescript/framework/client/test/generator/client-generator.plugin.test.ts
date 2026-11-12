import { describe, expect, test } from 'bun:test';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { api, application, endpoint, module, openapi } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { getCurrentProject, getProjectRoot, readFileContent } from '@putnami/utils';
import { ClientGeneratorPlugin, clientGenerator } from '../../src/generator/client-generator.plugin';

/**
 * clientGenerator() declares the application a client provider. It names no
 * target, output or package; its only effect is that the openapi() contract
 * names each operation's producer (tooling/clientgen-extension ADR 0007).
 */

const BUILD_OPTIONS = { publishCapabilityManifest: false, publishDesignGraph: false };

const widgets = {
  service: { id: 'widgets', audience: 'widgets' },
  credentials: { service: { kind: 'service-token' as const } },
};

const FEATURE = {
  id: 'catalog/widgets',
  name: 'Widgets',
  outcome: 'Callers can read widgets',
  owner: 'catalog',
};

/** A module serving two first-party operations under its own contract. */
function widgetsModule(feature: typeof FEATURE | undefined) {
  const surface = module('widgets');
  if (feature) surface.feature(feature);
  return surface
    .use(
      api({ autoScan: false, client: widgets })
        .register(
          '/widgets',
          endpoint(() => ({ total: 0 })),
          'GET',
        )
        .register(
          '/widgets/[id]',
          endpoint(() => ({ id: 'w' })),
          'GET',
        ),
    )
    .use(openapi({ title: 'Widgets', version: '1.0.0', output: false }));
}

// The apps hold no HTTP server: building one writes a serve entry into this
// package's package.json.
// biome-ignore lint/suspicious/noExplicitAny: the written spec is read back as plain JSON
async function buildContract(app: ReturnType<typeof application>): Promise<any> {
  const result = await app.build(BUILD_OPTIONS);
  const path = result.assets?.['schema/openapi.json'];
  if (!path) throw new Error('openapi() wrote no spec under schema/openapi.json');
  return JSON.parse(readFileContent(path, 'utf-8'));
}

// biome-ignore lint/suspicious/noExplicitAny: the written spec is read back as plain JSON
function clientMembers(spec: any): Record<string, Record<string, unknown>> {
  const members: Record<string, Record<string, unknown>> = {};
  for (const [path, item] of Object.entries(spec.paths as Record<string, Record<string, Record<string, unknown>>>)) {
    for (const [method, operation] of Object.entries(item)) {
      members[`${method.toUpperCase()} ${path}`] = operation['x-putnami-client'] as Record<string, unknown>;
    }
  }
  return members;
}

describe('clientGenerator()', () => {
  test('is a plugin that asks for operation attribution and nothing else', () => {
    const plugin = clientGenerator();
    expect(plugin).toBeInstanceOf(ClientGeneratorPlugin);
    expect({ ...plugin }).toEqual({ name: 'client-generator', attributesClientOperations: true });
  });

  specTest(
    "names each operation's producer in the contract and writes no client, manifest or config",
    {
      feature: 'tooling/cross-language-rest-clients',
      requirement: 'contract-carries-attribution',
      check: 'typescript-code-declarations-name-no-target',
    },
    async () => {
      const app = application().use(widgetsModule(FEATURE)).use(clientGenerator());

      const spec = await buildContract(app);

      const producer = { project: getCurrentProject().name, feature: FEATURE.id };
      const members = clientMembers(spec);
      expect(Object.keys(members)).toEqual(['GET /widgets', 'GET /widgets/{id}']);
      for (const member of Object.values(members)) {
        expect(member['producer']).toEqual(producer);
        // The producer is the last member, where the Go describer writes it.
        expect(Object.keys(member).at(-1)).toBe('producer');
      }
      const projectRoot = getProjectRoot();
      expect(existsSync(join(projectRoot, '.gen/clientgen/config.json'))).toBe(false);
      expect(existsSync(join(projectRoot, 'clients'))).toBe(false);
    },
  );

  test('leaves the contract unattributed without clientGenerator()', async () => {
    const app = application().use(widgetsModule(FEATURE));

    const members = clientMembers(await buildContract(app));

    expect(Object.keys(members)).toHaveLength(2);
    for (const member of Object.values(members)) expect(member).not.toHaveProperty('producer');
  });

  test('names no producer for a module that declares no feature', async () => {
    const app = application().use(widgetsModule(undefined)).use(clientGenerator());

    const members = clientMembers(await buildContract(app));

    expect(Object.keys(members)).toHaveLength(2);
    for (const member of Object.values(members)) expect(member).not.toHaveProperty('producer');
  });
});
