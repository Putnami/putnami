import { describe, expect, it } from 'bun:test';
import { HttpException, runInContext } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { application } from '../../src/application';
import type { HttpMiddleware } from '../../src/http/http-middleware.type';
import { HttpResponse } from '../../src/http/http-response';
import { http } from '../../src/http/http.plugin';
import { LoggerMiddleware } from '../../src/http/logger.middleware';
import { type MiddlewareForm, middlewareFormOf, middlewareFromForm } from '../../src/http/middleware-form';
import { OriginGuardMiddleware } from '../../src/http/origin-guard.middleware';
import { SecurityHeadersMiddleware } from '../../src/http/security-headers.middleware';
import { platform } from '../../src/platform';

/** What a middleware or a form gives for one request: its answer, or the error that went on. */
type Outcome = { response: HttpResponse | undefined } | { error: unknown };

/** The inner chain of a request: an answer, or an error it throws. */
type Inner = () => HttpResponse | undefined;

/** Runs `middleware` around `inner`, as the chain does. */
async function throughMiddleware(middleware: HttpMiddleware, ctx: unknown, inner: Inner): Promise<Outcome> {
  return runInContext(ctx as never, () =>
    middleware(ctx as never, async () => inner()).then(
      (response) => ({ response }),
      (error: unknown) => ({ error }),
    ),
  );
}

/** Runs `form`'s hooks around `inner` by hand, in the order the form's contract gives. */
async function throughForm(form: MiddlewareForm<unknown>, ctx: unknown, inner: Inner): Promise<Outcome> {
  return runInContext(ctx as never, async () => {
    const c = ctx as never;
    if (form.passes?.(c)) {
      try {
        return { response: inner() };
      } catch (error) {
        return { error };
      }
    }
    let state: unknown;
    try {
      const early = form.answer?.(c);
      if (early !== undefined) return { response: await early };
      state = form.enter?.(c);
    } catch (error) {
      return { error };
    }
    let response: HttpResponse | undefined;
    try {
      response = inner();
    } catch (error) {
      try {
        form.fail?.(c, error, state);
      } catch (thrown) {
        return { error: thrown };
      }
      return { error };
    }
    try {
      return { response: form.leave ? form.leave(c, response, state) : response };
    } catch (error) {
      return { error };
    }
  });
}

function formOf(middleware: HttpMiddleware): MiddlewareForm<unknown> {
  const form = middlewareFormOf(middleware);
  if (form === undefined) throw new Error('the middleware has no form');
  return form;
}

function headersOf(outcome: Outcome): Record<string, string> | unknown {
  if (!('response' in outcome)) return outcome.error;
  if (outcome.response === undefined) return undefined;
  const native = outcome.response.copy().get();
  return { status: native.status, ...Object.fromEntries(native.headers.entries()) };
}

function requestContext(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  const headers = new Headers((overrides['headers'] as Record<string, string> | undefined) ?? {});
  return {
    method: 'GET',
    route: '/items',
    path: () => '/items',
    host: () => 'app.example.com',
    secured: () => true,
    logger: new MemoryLogger(),
    ...overrides,
    headers,
  };
}

describe('middlewareFromForm', () => {
  it('runs passes, answer, enter, leave and fail in the order of the contract', async () => {
    const calls: string[] = [];
    const middleware = middlewareFromForm<number>({
      passes: () => {
        calls.push('passes');
        return false;
      },
      answer: () => {
        calls.push('answer');
        return undefined;
      },
      enter: () => {
        calls.push('enter');
        return 7;
      },
      leave: (_ctx, response, state) => {
        calls.push(`leave:${state}`);
        return response;
      },
      fail: (_ctx, _error, state) => {
        calls.push(`fail:${state}`);
      },
    });
    const ok = new HttpResponse('ok');
    expect(await throughMiddleware(middleware, {}, () => ok)).toEqual({ response: ok });
    expect(calls).toEqual(['passes', 'answer', 'enter', 'leave:7']);

    calls.length = 0;
    const boom = new Error('boom');
    expect(
      await throughMiddleware(middleware, {}, () => {
        throw boom;
      }),
    ).toEqual({ error: boom });
    expect(calls).toEqual(['passes', 'answer', 'enter', 'fail:7']);
  });

  it('leaves a request it passes untouched, and runs no other hook', async () => {
    let hooks = 0;
    const middleware = middlewareFromForm({
      passes: () => true,
      answer: () => {
        hooks++;
        return new HttpResponse('refused', { status: 403 });
      },
      leave: () => {
        hooks++;
        return undefined;
      },
    });
    const ok = new HttpResponse('ok');
    expect(await throughMiddleware(middleware, {}, () => ok)).toEqual({ response: ok });
    expect(hooks).toBe(0);
  });

  it('answers early without running the inner chain, enter or leave', async () => {
    const refused = new HttpResponse('refused', { status: 403 });
    let inner = 0;
    let hooks = 0;
    const middleware = middlewareFromForm({
      answer: () => refused,
      enter: () => {
        hooks++;
      },
      leave: () => {
        hooks++;
        return undefined;
      },
    });
    const outcome = await throughMiddleware(middleware, {}, () => {
      inner++;
      return new HttpResponse('ok');
    });
    expect(outcome).toEqual({ response: refused });
    expect(inner).toBe(0);
    expect(hooks).toBe(0);
  });

  it('hands an error thrown in leave to the middlewares outside, not to its own fail', async () => {
    const thrown = new Error('leave threw');
    let failed = 0;
    const middleware = middlewareFromForm({
      leave: () => {
        throw thrown;
      },
      fail: () => {
        failed++;
      },
    });
    expect(await throughMiddleware(middleware, {}, () => new HttpResponse('ok'))).toEqual({ error: thrown });
    expect(failed).toBe(0);
  });

  it('replaces the error with the one its fail throws', async () => {
    const replaced = new Error('fail threw');
    const middleware = middlewareFromForm({
      fail: () => {
        throw replaced;
      },
    });
    expect(
      await throughMiddleware(middleware, {}, () => {
        throw new Error('inner failed');
      }),
    ).toEqual({ error: replaced });
  });

  it('rejects an inner chain that throws synchronously, and resolves one that answers a plain value', async () => {
    const failure = new Error('thrown synchronously');
    const plain = new HttpResponse('ok');
    for (const form of [{}, { passes: () => true }, { leave: (_c: unknown, r: HttpResponse | undefined) => r }]) {
      const middleware = middlewareFromForm(form);
      const rejected = await middleware({} as never, () => {
        throw failure;
      }).then(
        () => 'resolved',
        (error: unknown) => error,
      );
      expect(rejected).toBe(failure);
      const resolved = await middleware({} as never, (() => plain) as unknown as () => Promise<HttpResponse>);
      expect(resolved).toBe(plain);
    }
  });
});

describe('built-in middleware forms', () => {
  const answers: Record<string, Inner> = {
    json: () => HttpResponse.json({ ok: true }),
    text: () => new HttpResponse('ok', { headers: { 'X-Frame-Options': 'SAMEORIGIN' } }),
    none: () => undefined,
    http: () => {
      throw new HttpException('conflict', 409);
    },
    error: () => {
      throw new Error('boom');
    },
  };

  /** Each built-in gives, through its form by hand, what it gives as a middleware. */
  async function expectParity(middleware: HttpMiddleware, contexts: Record<string, unknown>[]): Promise<void> {
    const form = formOf(middleware);
    for (const overrides of contexts) {
      for (const inner of Object.values(answers)) {
        const viaMiddleware = await throughMiddleware(middleware, requestContext(overrides), inner);
        const viaForm = await throughForm(form, requestContext(overrides), inner);
        expect(headersOf(viaForm)).toEqual(headersOf(viaMiddleware));
      }
    }
  }

  it('security headers: same headers, over HTTPS and plaintext, and on an opted-out route', async () => {
    await expectParity(SecurityHeadersMiddleware(), [
      {},
      { secured: () => false },
      { __matchedHandlers: [{ route: '/items', securityHeaders: false }] },
    ]);
    await expectParity(SecurityHeadersMiddleware({ frameOptions: 'SAMEORIGIN', contentSecurityPolicy: false }), [{}]);
  });

  it('origin guard: same answer for a safe, a same-origin, a cross-site and a reported request', async () => {
    const contexts = [
      { method: 'GET', headers: { Origin: 'https://evil.example' } },
      { method: 'POST', headers: { 'Sec-Fetch-Site': 'same-origin' } },
      { method: 'POST', headers: { 'Sec-Fetch-Site': 'cross-site', Origin: 'https://evil.example' } },
      { method: 'POST', headers: { Origin: 'https://evil.example' } },
      { method: 'POST', __matchedHandlers: [{ route: '/items', minimal: true }], headers: { Origin: 'https://x.io' } },
    ];
    await expectParity(OriginGuardMiddleware(), contexts);
    await expectParity(OriginGuardMiddleware({ mode: 'report' }), contexts);
    await expectParity(OriginGuardMiddleware({ trustedOrigins: ['https://evil.example'] }), contexts);
  });

  it('logger: same answer and same records, on a logged and on an excluded route', async () => {
    const middleware = LoggerMiddleware({ exclude: ['/health'] });
    const form = formOf(middleware);
    const records = (logger: MemoryLogger) =>
      logger.entries.map((entry) => ({
        level: entry.level,
        message: entry.message,
        status: (entry.context?.['http'] as Record<string, unknown> | undefined)?.['status'],
      }));
    for (const overrides of [{}, { route: '/health', path: () => '/health' }, { route: undefined }]) {
      for (const inner of Object.values(answers)) {
        const viaMiddlewareContext = requestContext(overrides);
        const viaFormContext = requestContext(overrides);
        const viaMiddleware = await throughMiddleware(middleware, viaMiddlewareContext, inner);
        const viaForm = await throughForm(form, viaFormContext, inner);
        expect(headersOf(viaForm)).toEqual(headersOf(viaMiddleware));
        expect(records(viaFormContext['logger'] as MemoryLogger)).toEqual(
          records(viaMiddlewareContext['logger'] as MemoryLogger),
        );
      }
    }
  });

  it('platform probes: a probe path answers through the form as through the middleware', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.get('/[page]', () => ({ page: true }));
    const app = application().use(httpPlugin).use(platform());
    await app.start();
    try {
      const base = `http://localhost:${httpPlugin.getServer()?.port}`;
      const livez = await fetch(`${base}/livez`);
      expect(livez.status).toBe(200);
      const page = await fetch(`${base}/about`);
      expect(await page.json()).toEqual({ page: true });
      const probes = (httpPlugin as unknown as { middlewares: HttpMiddleware[] }).middlewares.filter(
        (middleware) => middlewareFormOf(middleware)?.answer !== undefined,
      );
      expect(probes).toHaveLength(1);
      await expectParity(probes[0] as HttpMiddleware, [
        { route: '/[page]', path: () => '/about' },
        { route: '/[page]', path: () => '/livez' },
        { method: 'POST', route: '/[page]', path: () => '/livez' },
      ]);
    } finally {
      await app.stop();
    }
  });
});
