import { expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ClientContractOperation } from '@putnami/application';
import { BaseClient, bindServiceClient, type GeneratedServiceDescriptor } from '../../src';

const operation: ClientContractOperation = {
  stream: 'unary',
  transports: [{ protocol: 'rest-json', encoding: 'json', path: '/page' }],
  security: { alternatives: [{ allOf: [] }] },
  errors: [],
  idempotency: { kind: 'safe' },
};
const descriptor: GeneratedServiceDescriptor = {
  contract: { protocolVersion: 1, service: { id: 'page', audience: 'urn:page' }, credentials: {} },
  service: 'page',
  transport: 'http',
  operations: { page: operation },
};
class PageClient extends BaseClient {
  readonly serviceName = 'page';
  page() {
    return this.request<string>('GET', '/page', { operationId: 'page' });
  }
}

specTest(
  'caller-paths-match-the-shared-refusal-corpus',
  {
    feature: 'typescript/service-clients',
    requirement: 'caller-resolved-bindings',
    check: 'caller-paths-match-the-shared-refusal-corpus',
  },
  () => {
    const corpus = JSON.parse(
      readFileSync(
        resolve(import.meta.dir, '../../../../../protocols/clientcontract/fixtures/page/paths.json'),
        'utf8',
      ),
    ) as { valid: string[]; invalid: string[] };
    for (const path of corpus.valid) {
      const client = bindServiceClient(PageClient, descriptor, {
        url: 'https://example.com',
        clientId: 'replica',
        operationPaths: { page: path },
      });
      client.dispose();
    }
    for (const path of corpus.invalid)
      expect(() =>
        bindServiceClient(PageClient, descriptor, {
          url: 'https://example.com',
          clientId: 'replica',
          operationPaths: { page: path },
        }),
      ).toThrow();
    expect(() =>
      bindServiceClient(PageClient, descriptor, {
        url: 'https://example.com',
        clientId: 'replica',
        operationPaths: { unknown: '/x' },
      }),
    ).toThrow();
    for (const changed of [
      { ...operation, stream: 'server' },
      { ...operation, transports: [{ protocol: 'connect', encoding: 'json', path: '/page' }] },
      { ...operation, transports: [{ protocol: 'rest-json', encoding: 'json', path: '/page/{relation}' }] },
    ])
      expect(() =>
        bindServiceClient(
          PageClient,
          { ...descriptor, operations: { page: changed as ClientContractOperation } },
          { url: 'https://example.com', clientId: 'replica', operationPaths: { page: '/x' } },
        ),
      ).toThrow();
  },
);

specTest(
  'caller-bindings-snapshot-paths-isolate-caches-and-dispose',
  {
    feature: 'typescript/service-clients',
    requirement: 'caller-resolved-bindings',
    check: 'caller-bindings-snapshot-paths-isolate-caches-and-dispose',
  },
  async () => {
    const paths: string[] = [];
    const server = Bun.serve({
      port: 0,
      fetch(request) {
        const path = new URL(request.url).pathname;
        paths.push(path);
        return Response.json(path);
      },
    });
    const cacheDescriptor = {
      ...descriptor,
      operations: { page: { ...operation, resilience: { cache: { freshMs: 60_000 } } } },
    };
    const mapping = { page: '/one' };
    const one = bindServiceClient(PageClient, cacheDescriptor, {
      url: server.url.toString().replace(/\/$/, ''),
      clientId: 'replica',
      operationPaths: mapping,
    });
    const two = bindServiceClient(PageClient, cacheDescriptor, {
      url: server.url.toString().replace(/\/$/, ''),
      clientId: 'replica',
      operationPaths: { page: '/two' },
    });
    mapping.page = '/mutated';
    try {
      expect(await one.page()).toBe('/one');
      expect(await two.page()).toBe('/two');
      one.dispose();
      await expect(one.page()).rejects.toThrow();
      expect(paths).toEqual(['/one', '/two']);
    } finally {
      one.dispose();
      two.dispose();
      server.stop(true);
    }
  },
);
