import { describe, expect, it } from 'bun:test';
import type { HttpPlugin, RouteHandler } from '@putnami/application';
import { type HttpRequestContext, HttpResponse } from '@putnami/application';
import { HttpException } from '@putnami/runtime';
import type { RouteObject } from 'react-router';
import { runInContext } from '../../runtime/src/context/context.utils';
import { actionHandler } from '../src/ssr/handlers/action.handler';
import { registerRouteAction } from '../src/ssr/route-registration.utils';
import { ACTION_OUTCOME_CONTEXT_KEY, type ActionOutcomeSlot, contextSlots } from '../src/shared/context-slots';

const ROUTE = '/tasks/[id]';

const createContext = (route?: string) =>
  ({
    req: new Request('http://localhost/tasks/7'),
    headers: new Headers(),
    params: {},
    route,
    queryParams: () => ({}),
    body: async () => undefined,
    statusCode: 0,
  }) as unknown as HttpRequestContext;

const outcomeOf = (ctx: HttpRequestContext) =>
  contextSlots(ctx)[ACTION_OUTCOME_CONTEXT_KEY] as ActionOutcomeSlot | undefined;

describe('action outcome slot', () => {
  it('records ok with the matched route when the handler settles', async () => {
    const ctx = createContext(ROUTE);
    const wrapped = actionHandler(() => ({ id: 7 }));

    const response = await runInContext(ctx as never, async () => await wrapped());

    expect(await response.get().json()).toEqual({ id: 7 });
    expect(outcomeOf(ctx)).toEqual({ route: '/tasks/[id]', outcome: 'ok' });
  });

  it('leaves the route undefined when nothing matched', async () => {
    const ctx = createContext();
    const wrapped = actionHandler(() => undefined);

    const response = await runInContext(ctx as never, async () => await wrapped());

    expect(response.status).toBe(204);
    expect(outcomeOf(ctx)).toEqual({ route: undefined, outcome: 'ok' });
  });

  it('records validation_error for a 400 or 422 HttpException', async () => {
    for (const status of [400, 422]) {
      const ctx = createContext(ROUTE);
      const wrapped = actionHandler(() => {
        throw new HttpException({ error: 'bad input' }, status);
      });

      const response = await runInContext(ctx as never, async () => await wrapped());

      expect(response.status).toBe(status);
      expect(outcomeOf(ctx)).toEqual({ route: '/tasks/[id]', outcome: 'validation_error' });
    }
  });

  it('records error for any other HttpException status', async () => {
    const ctx = createContext(ROUTE);
    const wrapped = actionHandler(() => {
      throw new HttpException({ error: 'missing' }, 404);
    });

    const response = await runInContext(ctx as never, async () => await wrapped());

    expect(response.status).toBe(404);
    expect(outcomeOf(ctx)).toEqual({ route: '/tasks/[id]', outcome: 'error' });
  });

  it('records error and rethrows an unknown failure', async () => {
    const ctx = createContext(ROUTE);
    const wrapped = actionHandler(() => {
      throw new Error('boom');
    });

    await expect(runInContext(ctx as never, async () => await wrapped())).rejects.toThrow('boom');
    expect(outcomeOf(ctx)).toEqual({ route: '/tasks/[id]', outcome: 'error' });
  });
});

describe('action outcome slot — returned responses', () => {
  // Returning the failure is the documented way to reject a form submission
  // (doc/forms-and-actions.md), so these paths decide the outcome for most real
  // validation errors. Reading the outcome off the returned status rather than
  // off the fact that the handler returned is what keeps them honest.
  const cases: Array<{ name: string; build: () => unknown; want: ActionOutcomeSlot['outcome'] }> = [
    {
      name: 'a returned 400 is a validation error',
      build: () => HttpResponse.json({ errors: { title: 'required' } }, { status: 400 }),
      want: 'validation_error',
    },
    {
      name: 'a returned 422 is a validation error',
      build: () => HttpResponse.json({ errors: {} }, { status: 422 }),
      want: 'validation_error',
    },
    {
      name: 'a returned 404 is an error',
      build: () => new Response('Task not found', { status: 404 }),
      want: 'error',
    },
    {
      name: 'a returned 500 is an error',
      build: () => HttpResponse.json({ error: 'boom' }, { status: 500 }),
      want: 'error',
    },
    {
      name: 'a redirect is the normal success path for a form',
      build: () => HttpResponse.redirect('/tasks'),
      want: 'ok',
    },
    {
      name: 'a returned 200 response is ok',
      build: () => HttpResponse.json({ id: 7 }),
      want: 'ok',
    },
  ];

  for (const testCase of cases) {
    it(testCase.name, async () => {
      const ctx = createContext(ROUTE);
      const built = testCase.build();
      const wrapped = actionHandler(() => built);

      const response = await runInContext(ctx as never, async () => await wrapped());

      expect(outcomeOf(ctx)?.outcome).toBe(testCase.want);
      // Recording the outcome must not change what the action answers with:
      // the caller gets back the exact response object the handler built.
      expect(response).toBe(built);
    });
  }
});

describe('action outcome slot — the HTTP form route', () => {
  /**
   * Registers one eager action and returns the handler the HTTP route got.
   *
   * The React Router route node is not the server's path: a form POST, with or
   * without JavaScript, reaches the registered HTTP route. Wrapping only the
   * node left the slot unwritten on every real submission, and a reader of it
   * — the analytics form-submit row — observed nothing at all.
   */
  function registerAction(handler: (ctx: HttpRequestContext) => unknown): RouteHandler {
    let registered: RouteHandler | undefined;
    const httpPlugin = {
      post: (_route: string, routeHandler: RouteHandler) => {
        registered = routeHandler;
      },
    } as unknown as HttpPlugin;

    registerRouteAction({
      httpPlugin,
      cache: {} as never,
      route: ROUTE,
      httpRoute: '/tasks/:id',
      node: {} as RouteObject,
      action: { default: { handler } } as never,
      isLazy: false,
      middleware: undefined,
    });

    if (!registered) throw new Error('registerRouteAction registered no POST route');
    return registered;
  }

  it('publishes ok on the request context the middleware chain reads', async () => {
    const ctx = createContext(ROUTE);
    const route = registerAction(() => ({ task: { id: 7 } }));

    const result = await route(ctx);

    // The wrapper records and returns; it converts nothing.
    expect(result).toEqual({ task: { id: 7 } });
    expect(outcomeOf(ctx)).toEqual({ route: '/tasks/[id]', outcome: 'ok' });
  });

  it('publishes validation_error for a returned 422', async () => {
    const ctx = createContext(ROUTE);
    const route = registerAction(() => HttpResponse.json({ errors: {} }, { status: 422 }));

    await route(ctx);

    expect(outcomeOf(ctx)?.outcome).toBe('validation_error');
  });

  it('publishes error and rethrows when the action throws', async () => {
    const ctx = createContext(ROUTE);
    const route = registerAction(() => {
      throw new Error('boom');
    });

    await expect(route(ctx)).rejects.toThrow('boom');
    expect(outcomeOf(ctx)?.outcome).toBe('error');
  });
});
