import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';

describe('HttpPlugin HTTP route generation', () => {
  specTest(
    'emits direct and native routes from the build-time registry',
    {
      feature: 'typescript/http-services',
      requirement: 'route-fragments',
      check: 'pipeline-and-native-routes-reach-the-build-result',
    },
    async () => {
      const server = http()
        .get('/healthz', () => ({ ok: true }))
        .native('GET', '/version', new Response('1.0.0'))
        .native('/readyz', new Response('ok'));
      const result = await application().use(server).build();

      expect(result.httpRoutes).toEqual([
        expect.objectContaining({ path: '/healthz', methods: ['GET', 'HEAD'] }),
        expect.objectContaining({ path: '/version', methods: ['GET'] }),
        expect.objectContaining({
          path: '/readyz',
          methods: ['DELETE', 'GET', 'HEAD', 'OPTIONS', 'PATCH', 'POST', 'PUT'],
        }),
      ]);
    },
  );

  specTest(
    'omits implicit HEAD when automatic HTTP methods are disabled',
    {
      feature: 'typescript/http-services',
      requirement: 'route-fragments',
      check: 'secure-false-drops-the-implicit-head',
    },
    async () => {
      const result = await application()
        .use(http({ secure: false }).get('/healthz', () => ({ ok: true })))
        .build();

      expect(result.httpRoutes).toEqual([expect.objectContaining({ path: '/healthz', methods: ['GET'] })]);
    },
  );
});
