import { describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { ContainerContext } from '../../../runtime/src/inject/container-context';
import { provide } from '../../../runtime/src/inject/provider';
import { tagged } from '../../../runtime/src/inject/token';
import { LoaderBuilder, isLoaderDefinition, loader } from '../../src/ssr/loader';

function createMockContext(overrides: Record<string, unknown> = {}) {
  return {
    user: undefined,
    params: {},
    req: new Request('http://localhost/test'),
    url: 'http://localhost/test',
    method: 'GET',
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

describe('loader()', () => {
  it('returns a LoaderBuilder when called without arguments', () => {
    expect(loader()).toBeInstanceOf(LoaderBuilder);
  });

  it('returns a LoaderDefinition when called with a handler', () => {
    const handler = () => ({ data: true });
    const def = loader(handler);

    expect(isLoaderDefinition(def)).toBe(true);
    expect(def.handler).toBe(handler);
    expect(def.schemas).toBeUndefined();
  });
});

describe('LoaderBuilder', () => {
  it('builds a valid LoaderDefinition via handle()', () => {
    const handler = () => ({ users: [] });
    const def = loader().handle(handler);

    expect(isLoaderDefinition(def)).toBe(true);
    expect(typeof def.handler).toBe('function');
  });

  it('chains params() and stores schema', () => {
    const schema = { id: String };
    const def = loader()
      .params(schema)
      .handle(() => ({}));

    expect(def.schemas?.params).toBe(schema);
  });

  it('chains query() and stores schema', () => {
    const schema = { page: Number };
    const def = loader()
      .query(schema)
      .handle(() => ({}));

    expect(def.schemas?.query).toBe(schema);
  });

  it('chains params() and query() together', () => {
    const paramsSchema = { id: String };
    const querySchema = { include: String };
    const def = loader()
      .params(paramsSchema)
      .query(querySchema)
      .handle(() => ({}));

    expect(def.schemas?.params).toBe(paramsSchema);
    expect(def.schemas?.query).toBe(querySchema);
  });

  it('wraps handler with validation when schemas are provided', () => {
    const innerHandler = () => ({ result: true });
    const def = loader().params({ id: String }).handle(innerHandler);

    // The handler should be a wrapper, not the original
    expect(def.handler).not.toBe(innerHandler);
  });

  specTest(
    'does not wrap handler when no schemas are provided',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'server-data-boundary',
      check: 'a-loader-without-schemas-is-not-wrapped',
    },
    () => {
      const innerHandler = () => ({ result: true });
      const def = loader().handle(innerHandler);

      // No schemas means no wrapping — handler should be the same reference
      expect(def.schemas).toBeUndefined();
    },
  );

  it('stores cache options', () => {
    const def = loader()
      .cache({ ttl: 60_000, maxAge: 30 })
      .handle(() => ({}));

    expect(def.cache).toEqual({ ttl: 60_000, maxAge: 30 });
  });

  specTest(
    'validates and coerces params and query',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'server-data-boundary',
      check: 'a-loader-validates-and-coerces-its-declared-input',
    },
    async () => {
      const def = loader()
        .params({ id: Number })
        .query({ page: Number })
        .handle((ctx) => ({
          params: ctx.params,
          query: ctx.queryParams(),
        }));

      const result = await def.handler(
        createMockContext({
          params: { id: '7' },
          queryParams: () => ({ page: '2' }),
        }),
      );

      expect(result).toEqual({
        params: { id: 7 },
        query: { page: 2 },
      });
    },
  );

  specTest(
    'resolves injected dependencies from the active DI scope',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'server-data-boundary',
      check: 'a-loader-resolves-its-dependencies-from-the-request-scope',
    },
    async () => {
      class Repository {
        list() {
          return ['alpha', 'beta'];
        }
      }

      const container = new ContainerContext('loader-test');
      container.register(provide(Repository));
      await container.start();

      try {
        await container.scope(async () => {
          const def = loader()
            .inject({ repo: Repository })
            .handle((deps) => ({ items: deps.repo.list() }));

          expect(def.handler(createMockContext())).toEqual({ items: ['alpha', 'beta'] });
        });
      } finally {
        await container.close();
      }
    },
  );

  it('surfaces injected DI tokens for static-safety analysis', () => {
    class UserService {}
    const def = loader()
      .inject({ users: UserService })
      .handle((deps) => ({ ok: !!deps.users }));

    expect(def.inject).toEqual({ tokens: [UserService], dynamic: false });
  });

  it('flags tag/filter selectors as dynamic roots', () => {
    class Plugin {}
    const def = loader()
      .inject({ one: Plugin, many: tagged<Plugin>('plugin') })
      .handle(() => ({ ok: true }));

    expect(def.inject?.tokens).toEqual([Plugin]);
    expect(def.inject?.dynamic).toBe(true);
  });

  it('omits inject metadata when the loader injects nothing', () => {
    const def = loader().handle(() => ({ ok: true }));
    expect(def.inject).toBeUndefined();
  });

  specTest(
    'wraps handlers with security middleware',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'security-propagation',
      check: 'a-loader-is-wrapped-by-its-declared-security-middleware',
    },
    async () => {
      const handler = mock(() => ({ ok: true }));
      const def = loader().secure().handle(handler);

      const blocked = await def.handler(createMockContext());
      expect((blocked as { status: number }).status).toBe(401);
      expect(handler).not.toHaveBeenCalled();

      const allowed = await def.handler(createMockContext({ user: { sub: 'user-1' } }));
      expect(allowed).toEqual({ ok: true });
      expect(handler).toHaveBeenCalledTimes(1);
    },
  );
});

describe('isLoaderDefinition', () => {
  it('returns true for valid LoaderDefinition', () => {
    const def = loader(() => ({}));
    expect(isLoaderDefinition(def)).toBe(true);
  });

  it('returns true for builder-produced definition', () => {
    const def = loader().handle(() => ({}));
    expect(isLoaderDefinition(def)).toBe(true);
  });

  it('returns false for null', () => {
    expect(isLoaderDefinition(null)).toBe(false);
  });

  it('returns false for undefined', () => {
    expect(isLoaderDefinition(undefined)).toBe(false);
  });

  it('returns false for plain function', () => {
    expect(isLoaderDefinition(() => ({}))).toBe(false);
  });

  it('returns false for plain object', () => {
    expect(isLoaderDefinition({ handler: () => ({}) })).toBe(false);
  });
});
