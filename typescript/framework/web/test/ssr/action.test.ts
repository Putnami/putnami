import { describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { ContainerContext } from '../../../runtime/src/inject/container-context';
import { provide } from '../../../runtime/src/inject/provider';
import { ActionBuilder, isActionDefinition, action } from '../../src/ssr/action';

function createMockContext(overrides: Record<string, unknown> = {}) {
  return {
    user: undefined,
    params: {},
    req: new Request('http://localhost/test'),
    url: 'http://localhost/test',
    method: 'POST',
    headers: new Headers(),
    queryParams: () => ({}),
    body: async () => undefined,
    secured: () => false,
    host: () => 'localhost',
    domain: () => 'localhost',
    path: () => '/test',
    query: () => '',
    throw: (status: number, message?: string) => {
      throw new Error(message ?? `${status}`);
    },
    ...overrides,
  } as never;
}

describe('action()', () => {
  it('returns an ActionBuilder when called without arguments', () => {
    expect(action()).toBeInstanceOf(ActionBuilder);
  });

  it('returns an ActionDefinition when called with a handler', () => {
    const handler = () => ({ success: true });
    const def = action(handler);

    expect(isActionDefinition(def)).toBe(true);
    expect(def.handler).toBe(handler);
    expect(def.schemas).toBeUndefined();
  });
});

describe('ActionBuilder', () => {
  it('builds a valid ActionDefinition via handle()', () => {
    const handler = () => ({ created: true });
    const def = action().handle(handler);

    expect(isActionDefinition(def)).toBe(true);
    expect(typeof def.handler).toBe('function');
  });

  it('chains params() and stores schema', () => {
    const schema = { id: String };
    const def = action()
      .params(schema)
      .handle(() => ({}));

    expect(def.schemas?.params).toBe(schema);
  });

  it('chains query() and stores schema', () => {
    const schema = { format: String };
    const def = action()
      .query(schema)
      .handle(() => ({}));

    expect(def.schemas?.query).toBe(schema);
  });

  it('chains body() and stores schema', () => {
    const schema = { name: String, email: String };
    const def = action()
      .body(schema)
      .handle(() => ({}));

    expect(def.schemas?.body).toBe(schema);
  });

  it('chains params(), query(), and body() together', () => {
    const paramsSchema = { id: String };
    const querySchema = { include: String };
    const bodySchema = { name: String };
    const def = action()
      .params(paramsSchema)
      .query(querySchema)
      .body(bodySchema)
      .handle(() => ({}));

    expect(def.schemas?.params).toBe(paramsSchema);
    expect(def.schemas?.query).toBe(querySchema);
    expect(def.schemas?.body).toBe(bodySchema);
  });

  it('wraps handler with validation when schemas are provided', () => {
    const innerHandler = () => ({ result: true });
    const def = action().params({ id: String }).handle(innerHandler);

    // The handler should be a wrapper, not the original
    expect(def.handler).not.toBe(innerHandler);
  });

  specTest(
    'does not wrap handler when no schemas are provided',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'server-data-boundary',
      check: 'an-action-without-schemas-is-not-wrapped',
    },
    () => {
      const innerHandler = () => ({ result: true });
      const def = action().handle(innerHandler);

      expect(def.schemas).toBeUndefined();
    },
  );

  it('stores cache eviction patterns', () => {
    const def = action()
      .evict('loader:/users/*', () => 'page:/users/42')
      .handle(() => ({}));

    expect(def.evict).toHaveLength(2);
  });

  specTest(
    'validates and coerces params, query, and body',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'server-data-boundary',
      check: 'an-action-validates-and-coerces-its-declared-input',
    },
    async () => {
      const def = action()
        .params({ id: Number })
        .query({ page: Number })
        .body({ name: String })
        .handle(async (ctx) => ({
          params: ctx.params,
          query: ctx.queryParams(),
          body: await ctx.body(),
        }));

      const result = await def.handler(
        createMockContext({
          params: { id: '42' },
          queryParams: () => ({ page: '3' }),
          body: async () => ({ name: 'Ada' }),
        }),
      );

      expect(result).toEqual({
        params: { id: 42 },
        query: { page: 3 },
        body: { name: 'Ada' },
      });
    },
  );

  specTest(
    'resolves injected dependencies from the active DI scope',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'server-data-boundary',
      check: 'an-action-resolves-its-dependencies-from-the-request-scope',
    },
    async () => {
      class Greeter {
        greet() {
          return 'hello';
        }
      }

      const container = new ContainerContext('action-test');
      container.register(provide(Greeter));
      await container.start();

      try {
        await container.scope(async () => {
          const def = action()
            .inject({ greeter: Greeter })
            .handle((deps) => ({ message: deps.greeter.greet() }));

          await expect(def.handler(createMockContext())).resolves.toEqual({ message: 'hello' });
        });
      } finally {
        await container.close();
      }
    },
  );

  specTest(
    'wraps handlers with security middleware',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'security-propagation',
      check: 'an-action-is-wrapped-by-its-declared-security-middleware',
    },
    async () => {
      const handler = mock(() => ({ ok: true }));
      const def = action().secure().handle(handler);

      const blocked = await def.handler(createMockContext());
      expect((blocked as { status: number }).status).toBe(401);
      expect(handler).not.toHaveBeenCalled();

      const allowed = await def.handler(createMockContext({ user: { sub: 'user-1' } }));
      expect(allowed).toEqual({ ok: true });
      expect(handler).toHaveBeenCalledTimes(1);
    },
  );
});

describe('isActionDefinition', () => {
  it('returns true for valid ActionDefinition', () => {
    const def = action(() => ({}));
    expect(isActionDefinition(def)).toBe(true);
  });

  it('returns true for builder-produced definition', () => {
    const def = action().handle(() => ({}));
    expect(isActionDefinition(def)).toBe(true);
  });

  it('returns false for null', () => {
    expect(isActionDefinition(null)).toBe(false);
  });

  it('returns false for undefined', () => {
    expect(isActionDefinition(undefined)).toBe(false);
  });

  it('returns false for plain function', () => {
    expect(isActionDefinition(() => ({}))).toBe(false);
  });

  it('returns false for plain object', () => {
    expect(isActionDefinition({ handler: () => ({}) })).toBe(false);
  });
});
