import React from 'react';
import { describe, expect, it } from 'bun:test';
import type { HttpPlugin, HttpRequestContext, HttpResponse, RouteHandler } from '@putnami/application';
import { BadRequestException, ForbiddenException, HttpException, NotFoundException } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { useRouteError, type RouteObject } from 'react-router';
import { buildHttpContext } from '../../../application/src/http/http-context.builder';
import { runInContext } from '../../../runtime/src/context/context.utils';
import RootHtml from '../../src/ssr/default.html';
import { LOADER_REQUEST_HEADER } from '../../src/shared/client-redirect';
import { loaderErrorData } from '../../src/ssr/loader-error';
import { pageRenderer } from '../../src/ssr/page.renderer';
import { registerRouteLoader } from '../../src/ssr/route-registration.utils';

/** Registers a loader and returns the handler of its JSON endpoint. */
function loaderEndpoint(handler: (ctx: HttpRequestContext) => unknown, suffix = '.json'): RouteHandler {
  let registered: RouteHandler | undefined;
  const httpPlugin = {
    get: (_route: string, routeHandler: RouteHandler) => {
      registered = routeHandler;
    },
  } as unknown as HttpPlugin;
  const loaderModule = { default: { handler } };
  registerRouteLoader({
    httpPlugin,
    cache: { loadModule: async () => loaderModule } as never,
    route: '/tasks',
    httpRoute: '/tasks',
    node: {} as RouteObject,
    loader: loaderModule as never,
    isLazy: false,
    cacheKeyPrefix: 'loader',
    suffix,
    middleware: undefined,
  });
  if (!registered) throw new Error('registerRouteLoader registered no GET route');
  return registered;
}

function context(url: string): HttpRequestContext {
  const req = new Request(url, { headers: { [LOADER_REQUEST_HEADER]: '1' } });
  return Object.assign(buildHttpContext<HttpRequestContext>({ req }), { params: {}, statusCode: 0 }) as never;
}

/** What the loader endpoint answers on a client navigation when the loader throws `exception`. */
async function endpointAnswer(exception: unknown, suffix?: string) {
  const ctx = context('http://localhost/tasks.json');
  const route = loaderEndpoint(() => {
    throw exception;
  }, suffix);
  const response = (await runInContext(ctx, async () => await route(ctx))) as HttpResponse;
  return { response, body: await response.get().json(), ctx };
}

/** Renders the route error's `data` the boundary receives on a full page load. */
function ErrorData() {
  const error = useRouteError() as { data?: unknown };
  return React.createElement('pre', { id: 'error-data' }, JSON.stringify(error.data));
}

/** The `data` the error boundary gets on a full page load when the loader throws `exception`. */
async function ssrErrorData(exception: unknown): Promise<unknown> {
  const ctx = context('http://localhost/tasks');
  const handler = pageRenderer(
    () => [
      {
        path: '/tasks',
        loader: () => {
          throw exception;
        },
        element: React.createElement('div', null, 'Tasks'),
        errorElement: React.createElement(ErrorData),
      } as never,
    ],
    () => RootHtml,
    () => 'hydrate.js',
    500,
  );
  const response = (await runInContext(ctx, async () => await handler(ctx))) as HttpResponse;
  const html = await response.get().text();
  const rendered = /<pre id="error-data">([^<]*)<\/pre>/.exec(html)?.[1];
  if (rendered === undefined) throw new Error('the error boundary did not render');
  return JSON.parse(rendered.replaceAll('&quot;', '"').replaceAll('&#x27;', "'").replaceAll('&amp;', '&'));
}

const shapes: [string, () => HttpException][] = [
  ['a string message', () => new HttpException('Post not found', 404)],
  ['an object response', () => new ForbiddenException({ message: 'not an admin', table: 'secret_admins' })],
  ['an Error cause', () => new HttpException(new Error('relation "secret_admins" does not exist'), 400)],
  ['an Error cause given to a named exception', () => new BadRequestException(new Error('db secret'))],
  ['no message', () => new HttpException({ table: 'secret_admins' }, 409)],
  ['an array response', () => new HttpException(['secret_admins'], 422)],
  ['a status outside the table', () => new HttpException('teapot brewing', 499)],
];

describe('loader error projection', () => {
  for (const [shape, build] of shapes) {
    it(`answers the SSR error data on the loader endpoint for ${shape}`, async () => {
      const { response, body } = await endpointAnswer(build());

      expect(response.status).toBe(build().getStatus());
      expect(body).toEqual(await ssrErrorData(build()));
      expect(Object.keys(body as object).sort()).toEqual(['error', 'message', 'statusCode']);
      expect(JSON.stringify(body)).not.toContain('secret');
    });
  }

  specTest(
    'a loader endpoint answers the error projection of a full page load',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-response',
      check: 'a-loader-endpoint-answers-the-error-projection-of-a-full-page-load',
    },
    async () => {
      const exception = new NotFoundException({ message: 'rows out of range', table: 'rows' });
      const page = await endpointAnswer(exception);
      const layout = await endpointAnswer(exception, '-layout.json');

      expect(page.response.status).toBe(404);
      expect(page.body).toEqual({ statusCode: 404, error: 'Not Found', message: 'rows out of range' });
      expect(layout.body).toEqual(page.body);
      expect(await ssrErrorData(exception)).toEqual(page.body);
      // Recorded as the request error, as on a full page load.
      expect((page.ctx as unknown as { __requestError?: unknown }).__requestError).toBe(exception);
    },
  );

  it("keeps the exception's own message when it equals its cause's", () => {
    const exception = new HttpException('Post not found', 404, { cause: new Error('Post not found') });

    expect(loaderErrorData(exception)).toEqual({ statusCode: 404, error: 'Not Found', message: 'Post not found' });
  });

  it('leaves an error that is no HttpException, or below 400, to the dispatcher', async () => {
    const failure = new Error('db secret');
    await expect(endpointAnswer(failure)).rejects.toBe(failure);
    const notModified = new HttpException('Not Modified', 304);
    await expect(endpointAnswer(notModified)).rejects.toBe(notModified);
  });

  it('leaves the exception of a request that ended to the dispatcher', async () => {
    const ctx = context('http://localhost/tasks.json');
    const controller = new AbortController();
    controller.abort();
    Object.assign(ctx, { signal: controller.signal });
    const exception = new HttpException('late', 404);
    const route = loaderEndpoint(() => {
      throw exception;
    });

    await expect(runInContext(ctx, async () => await route(ctx))).rejects.toBe(exception);
  });
});
