import { afterAll, beforeAll, describe, test } from 'bun:test';
import { checkSpecDrift } from '../../src/runtime/spec-drift';

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;

const specContent = '{"openapi":"3.0.3","info":{"title":"Test","version":"1.0.0"},"paths":{}}';
// Pre-compute the hash of specContent
const specHasher = new Bun.CryptoHasher('sha256');
specHasher.update(specContent);
const correctHash = specHasher.digest('hex').substring(0, 16);

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    async fetch(req) {
      const url = new URL(req.url);

      if (url.pathname === '/_/openapi.json') {
        return new Response(specContent, {
          headers: { 'Content-Type': 'application/json' },
        });
      }

      if (url.pathname === '/_/api.proto') {
        return new Response('syntax = "proto3";', {
          headers: { 'Content-Type': 'text/plain' },
        });
      }

      return new Response('Not found', { status: 404 });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

describe('checkSpecDrift', () => {
  test('no warning when hash matches', async () => {
    // Should complete without throwing
    await checkSpecDrift(baseUrl, correctHash, 'http');
  });

  test('warns on hash mismatch (does not throw)', async () => {
    // Mismatched hash — should log a warning but not throw
    await checkSpecDrift(baseUrl, 'wrong-hash-value', 'http');
  });

  test('handles unreachable service gracefully', async () => {
    await checkSpecDrift('http://localhost:1', 'some-hash', 'http');
  });

  test('fetches correct endpoint for connect transport', async () => {
    // Proto endpoint for connect transport
    const protoHasher = new Bun.CryptoHasher('sha256');
    protoHasher.update('syntax = "proto3";');
    const protoHash = protoHasher.digest('hex').substring(0, 16);

    await checkSpecDrift(baseUrl, protoHash, 'connect');
  });

  test('skips drift comparison when the spec endpoint returns a non-OK response', async () => {
    const failingServer = Bun.serve({
      port: 0,
      fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === '/_/openapi.json') {
          return new Response('upstream unavailable', { status: 503 });
        }
        return new Response('Not found', { status: 404 });
      },
    });

    try {
      await checkSpecDrift(`http://localhost:${failingServer.port}`, 'unused-hash', 'http');
    } finally {
      failingServer.stop(true);
    }
  });
});
