import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import type { Application } from '../../src/application/application';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';
import { requireClient } from '../../src/http/require-client.middleware';
import { HttpResponse } from '../../src/http/http-response';

let app: Application;
let baseUrl: string;

beforeAll(async () => {
  app = application().use(
    (() => {
      const h = http({ port: 0 });
      h.use(requireClient(['orders-service', 'billing-service']));
      h.get(
        '/internal/data',
        () =>
          new HttpResponse(JSON.stringify({ secret: 'data' }), {
            headers: { 'Content-Type': 'application/json' },
          }),
      );
      return h;
    })(),
  );
  await app.start();

  // Get the actual port from the server
  const httpPlugin = app.getPlugins()[0] as InstanceType<typeof import('../../src/http/http.plugin').HttpPlugin>;
  const server = httpPlugin.getServer();
  baseUrl = `http://localhost:${server?.port}`;
});

afterAll(async () => {
  await app.stop();
});

describe('requireClient', () => {
  test('allows request from authorized client', async () => {
    const res = await fetch(`${baseUrl}/internal/data`, {
      headers: { 'X-Client-Id': 'orders-service' },
    });
    expect(res.status).toBe(200);
  });

  test('allows request from another authorized client', async () => {
    const res = await fetch(`${baseUrl}/internal/data`, {
      headers: { 'X-Client-Id': 'billing-service' },
    });
    expect(res.status).toBe(200);
  });

  test('rejects request from unauthorized client', async () => {
    const res = await fetch(`${baseUrl}/internal/data`, {
      headers: { 'X-Client-Id': 'evil-service' },
    });
    expect(res.status).toBe(403);
  });

  test('rejects request with no X-Client-Id header', async () => {
    const res = await fetch(`${baseUrl}/internal/data`);
    expect(res.status).toBe(403);
  });
});
