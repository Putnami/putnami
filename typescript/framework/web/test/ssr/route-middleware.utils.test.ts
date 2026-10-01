import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { HttpResponse } from '@putnami/application';
import { composeMiddleware, wrapWithMiddleware } from '../../src/ssr/route-middleware.utils';

describe('route-middleware.utils', () => {
  describe('composeMiddleware', () => {
    const mw1 = async (_ctx: unknown, next: () => Promise<unknown>) => next();
    const mw2 = async (_ctx: unknown, next: () => Promise<unknown>) => next();

    specTest(
      'returns undefined when both are undefined',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'security-propagation',
        check: 'a-route-outside-the-declaring-subtree-is-unwrapped',
      },
      () => {
        expect(composeMiddleware(undefined, undefined)).toBeUndefined();
      },
    );

    it('returns page middleware when layout is undefined', () => {
      const result = composeMiddleware(undefined, [mw1]);
      expect(result).toEqual([mw1]);
    });

    it('returns layout middleware when page is undefined', () => {
      const result = composeMiddleware([mw1], undefined);
      expect(result).toEqual([mw1]);
    });

    specTest(
      'merges layout and page middleware in order',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'security-propagation',
        check: 'layout-and-page-middleware-merge-in-order',
      },
      () => {
        const result = composeMiddleware([mw1], [mw2]);
        expect(result).toEqual([mw1, mw2]);
      },
    );
  });

  describe('wrapWithMiddleware', () => {
    const createCtx = () => ({ statusCode: 202 }) as never;

    specTest(
      'serializes plain object responses to JSON',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'ssr-response',
        check: 'a-plain-object-result-is-serialized-to-json',
      },
      async () => {
        const wrapped = wrapWithMiddleware(async () => ({ ok: true }), []);
        const response = await wrapped(createCtx());

        expect(response).toBeInstanceOf(HttpResponse);
        expect(await response?.get().json()).toEqual({ ok: true });
        expect(response?.status).toBe(202);
      },
    );

    it('serializes string responses to text/plain', async () => {
      const wrapped = wrapWithMiddleware(async () => 'hello', []);
      const response = await wrapped(createCtx());
      const native = response?.get();

      expect(await native?.text()).toBe('hello');
      expect(native?.headers.get('content-type')).toBe('text/plain');
    });

    it('preserves HttpResponse results returned by the handler', async () => {
      const wrapped = wrapWithMiddleware(async () => new HttpResponse('ready', { status: 204 }), []);
      const response = await wrapped(createCtx());

      expect(response?.status).toBe(204);
      expect(await response?.get().text()).toBe('ready');
    });

    specTest(
      'runs middleware in order and allows short-circuit responses',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'ssr-response',
        check: 'a-middleware-stack-runs-in-order-and-can-short-circuit',
      },
      async () => {
        const events: string[] = [];
        const wrapped = wrapWithMiddleware(async () => {
          events.push('handler');
          return { ok: true };
        }, [
          async (_ctx, next) => {
            events.push('mw1-before');
            const result = await next();
            events.push('mw1-after');
            return result;
          },
          async () => {
            events.push('mw2-stop');
            return new HttpResponse('blocked', { status: 403 });
          },
        ]);

        const response = await wrapped(createCtx());

        expect(events).toEqual(['mw1-before', 'mw2-stop', 'mw1-after']);
        expect(response?.status).toBe(403);
        expect(await response?.get().text()).toBe('blocked');
      },
    );
  });
});
