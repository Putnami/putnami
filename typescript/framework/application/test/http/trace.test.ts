import { describe, expect, it } from 'bun:test';
import { type Context, useContext } from '@putnami/runtime';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import { trace } from '../../src/http/trace.plugin';
import { application } from '../../src/application';

describe('Trace Middleware', () => {
  it('should generate a trace id if missing', async () => {
    let capturedTraceId: string | undefined;

    const httpPlugin = http({ port: 0 });
    httpPlugin.use(async (ctx) => {
      capturedTraceId = ctx.traceId;
      return new HttpResponse('ok');
    });

    const app = application().use(httpPlugin).use(trace());

    await app.start();
    const server = httpPlugin.getServer();
    const res = await fetch(`http://localhost:${server?.port}/`);

    expect(capturedTraceId).toBeDefined();
    expect(res.headers.get('X-Correlation-ID')).toBe(capturedTraceId!);
    await app.stop();
  });

  it('should use existing X-Correlation-ID', async () => {
    const existingId = 'test-id-123';
    let capturedTraceId: string | undefined;

    const httpPlugin = http({ port: 0 });
    httpPlugin.use(async (ctx) => {
      capturedTraceId = ctx.traceId;
      return new HttpResponse('ok');
    });

    const app = application().use(httpPlugin).use(trace());

    await app.start();
    const server = httpPlugin.getServer();
    const res = await fetch(`http://localhost:${server?.port}/`, {
      headers: { 'X-Correlation-ID': existingId },
    });

    expect(capturedTraceId).toBe(existingId);
    expect(res.headers.get('X-Correlation-ID')).toBe(existingId);
    await app.stop();
  });

  it('should extract trace id from GCP X-Cloud-Trace-Context', async () => {
    const traceId = '105445aa7843bc8bf206b12000100000';
    const spanId = '1';
    const gcpHeader = `${traceId}/${spanId};o=1`;
    let capturedTraceId: string | undefined;

    const httpPlugin = http({ port: 0 });
    httpPlugin.use(async (ctx) => {
      capturedTraceId = ctx.traceId;
      return new HttpResponse('ok');
    });

    const app = application().use(httpPlugin).use(trace());

    await app.start();
    const server = httpPlugin.getServer();
    const res = await fetch(`http://localhost:${server?.port}/`, {
      headers: { 'X-Cloud-Trace-Context': gcpHeader },
    });

    expect(capturedTraceId).toBe(traceId);
    // Should still set X-Correlation-ID in response
    expect(res.headers.get('X-Correlation-ID')).toBe(traceId);
    await app.stop();
  });

  it('should prioritize GCP header over X-Correlation-ID', async () => {
    const traceId = 'gcp-id';
    const otherId = 'correlation-id';
    let capturedTraceId: string | undefined;

    const httpPlugin = http({ port: 0 });
    httpPlugin.use(async (ctx) => {
      capturedTraceId = ctx.traceId;
      return new HttpResponse('ok');
    });

    const app = application().use(httpPlugin).use(trace());

    await app.start();
    const server = httpPlugin.getServer();
    const _res = await fetch(`http://localhost:${server?.port}/`, {
      headers: {
        'X-Cloud-Trace-Context': `${traceId}/1`,
        'X-Correlation-ID': otherId,
      },
    });

    expect(capturedTraceId).toBe(traceId);
    await app.stop();
  });

  it('should set traceId in logger context', async () => {
    let loggerTraceId: string | undefined;

    const httpPlugin = http({ port: 0 });
    httpPlugin.use(async (_ctx, _next) => {
      // manual check if context has traceId available via useContext() used by logger
      const c = useContext<Context>();
      loggerTraceId = c.traceId;
      return new HttpResponse('ok');
    });

    const app = application().use(httpPlugin).use(trace());

    await app.start();
    const server = httpPlugin.getServer();
    await fetch(`http://localhost:${server?.port}/`);

    expect(loggerTraceId).toBeDefined();
    await app.stop();
  });
});
