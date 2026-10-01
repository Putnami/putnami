import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { api, platform } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';

describe('hello-world sample', () => {
  let testApp: TestApp;

  beforeAll(async () => {
    testApp = await createTestApp({
      plugins: [platform(), api({ scanPath: 'src/api' })],
    });
  });

  afterAll(async () => {
    await testApp.stop();
  });

  it('should return a welcome message at GET /', async () => {
    const res = await testApp.fetch('/');
    expect(res.status).toBe(200);

    const data = await res.json();
    expect(data.message).toBe('Hello from Putnami!');
    expect(data.timestamp).toBeDefined();
  });

  it('should expose a health check endpoint', async () => {
    const res = await testApp.fetch('/healthz');
    expect(res.status).toBe(200);
  });
});
