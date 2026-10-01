import { describe, expect, it } from 'bun:test';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';

describe('HttpPlugin HTTP route generation', () => {
  it('emits direct and native routes from the build-time registry', async () => {
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
  });

  it('omits implicit HEAD when automatic HTTP methods are disabled', async () => {
    const result = await application()
      .use(http({ secure: false }).get('/healthz', () => ({ ok: true })))
      .build();

    expect(result.httpRoutes).toEqual([expect.objectContaining({ path: '/healthz', methods: ['GET'] })]);
  });
});
