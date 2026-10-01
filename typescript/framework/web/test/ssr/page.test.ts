import { describe, expect, it, mock } from 'bun:test';
import { error, isErrorDefinition } from '../../src/ssr/error';
import { isLayoutDefinition, layout, LayoutBuilder } from '../../src/ssr/layout';
import { isNotFoundDefinition, notFound } from '../../src/ssr/not-found';
import { isPageDefinition, PageBuilder, page } from '../../src/ssr/page';

const MockComponent = () => null;

describe('page()', () => {
  it('returns a PageBuilder', () => {
    expect(page()).toBeInstanceOf(PageBuilder);
  });
});

describe('PageBuilder', () => {
  it('renders a valid PageDefinition', () => {
    const def = page().render(MockComponent);

    expect(isPageDefinition(def)).toBe(true);
    expect(def.component).toBe(MockComponent);
    expect(def.middleware).toEqual([]);
    expect(def.statusCode).toBeUndefined();
  });

  it('chains secure() and produces middleware', () => {
    const def = page()
      .secure({ roles: ['admin'] })
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(typeof def.middleware[0]).toBe('function');
  });

  it('chains rateLimit() and produces middleware', () => {
    const def = page().rateLimit({ max: 100 }).render(MockComponent);

    expect(def.middleware).toHaveLength(1);
  });

  it('chains multiple middleware calls', () => {
    const def = page()
      .secure({ roles: ['user'] })
      .rateLimit({ max: 50 })
      .render(MockComponent);

    expect(def.middleware).toHaveLength(2);
  });

  it('sets status code via status()', () => {
    const def = page().status(403).render(MockComponent);

    expect(def.statusCode).toBe(403);
  });

  it('accepts custom middleware via use()', () => {
    const customMiddleware = mock(async (_ctx: unknown, next: () => Promise<unknown>) => next());
    const def = page()
      .use(customMiddleware as never)
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(def.middleware[0]).toBe(customMiddleware);
  });

  it('returns a new middleware array (not shared reference)', () => {
    const builder = page().secure();
    const def1 = builder.render(MockComponent);
    const def2 = builder.render(MockComponent);

    expect(def1.middleware).not.toBe(def2.middleware);
    expect(def1.middleware).toEqual(def2.middleware);
  });

  it('chains cors() and produces middleware', () => {
    const def = page().cors({ origin: 'https://example.com' }).render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(typeof def.middleware[0]).toBe('function');
  });

  it('surfaces a declarative security requirement after secure()', () => {
    const def = page()
      .secure({ roles: ['admin'], scopesAny: ['docs:read'] })
      .render(MockComponent);

    expect(def.security).toEqual({
      authenticated: true,
      roles: ['admin'],
      scopesAny: ['docs:read'],
    });
  });

  it('marks function guards as indeterminate so client gating default-denies', () => {
    const def = page()
      .secure(() => true)
      .render(MockComponent);

    expect(def.security).toEqual({ authenticated: true, indeterminate: true });
  });

  it('omits security when secure() was never called', () => {
    const def = page().rateLimit({ max: 100 }).render(MockComponent);

    expect(def.security).toBeUndefined();
  });
});

describe('isPageDefinition', () => {
  it('returns true for valid PageDefinition', () => {
    const def = page().render(MockComponent);
    expect(isPageDefinition(def)).toBe(true);
  });

  it('returns false for null', () => {
    expect(isPageDefinition(null)).toBe(false);
  });

  it('returns false for undefined', () => {
    expect(isPageDefinition(undefined)).toBe(false);
  });

  it('returns false for plain object', () => {
    expect(isPageDefinition({ middleware: [] })).toBe(false);
  });

  it('returns false for endpoint-like object', () => {
    expect(isPageDefinition({ __endpoint: 'putnami:endpoint' })).toBe(false);
  });
});

describe('layout()', () => {
  it('returns a LayoutBuilder', () => {
    expect(layout()).toBeInstanceOf(LayoutBuilder);
  });

  it('renders a valid LayoutDefinition', () => {
    const def = layout().render(MockComponent);

    expect(isLayoutDefinition(def)).toBe(true);
    expect(def.component).toBe(MockComponent);
    expect(def.middleware).toEqual([]);
  });

  it('chains secure() and produces middleware', () => {
    const def = layout()
      .secure({ roles: ['admin'] })
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(typeof def.middleware[0]).toBe('function');
  });

  it('chains multiple middleware calls', () => {
    const def = layout()
      .secure({ roles: ['user'] })
      .rateLimit({ max: 50 })
      .render(MockComponent);

    expect(def.middleware).toHaveLength(2);
  });

  it('accepts custom middleware via use()', () => {
    const customMiddleware = mock(async (_ctx: unknown, next: () => Promise<unknown>) => next());
    const def = layout()
      .use(customMiddleware as never)
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(def.middleware[0]).toBe(customMiddleware);
  });

  it('chains cors() and produces middleware', () => {
    const def = layout().cors().render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(typeof def.middleware[0]).toBe('function');
  });

  it('surfaces a declarative security requirement after secure()', () => {
    const def = layout()
      .secure({ roles: ['admin'] })
      .render(MockComponent);

    expect(def.security).toEqual({
      authenticated: true,
      roles: ['admin'],
    });
  });
});

describe('isLayoutDefinition', () => {
  it('returns true for valid LayoutDefinition', () => {
    const def = layout().render(MockComponent);
    expect(isLayoutDefinition(def)).toBe(true);
  });

  it('returns false for null', () => {
    expect(isLayoutDefinition(null)).toBe(false);
  });

  it('returns false for plain component', () => {
    expect(isLayoutDefinition(MockComponent)).toBe(false);
  });
});

describe('error()', () => {
  it('renders a valid ErrorDefinition', () => {
    const def = error().render(MockComponent);

    expect(isErrorDefinition(def)).toBe(true);
    expect(def.component).toBe(MockComponent);
    expect(def.middleware).toEqual([]);
  });

  it('chains middleware and status', () => {
    const def = error()
      .secure({ roles: ['admin'] })
      .status(503)
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(def.statusCode).toBe(503);
  });

  it('surfaces a declarative security requirement after secure()', () => {
    const def = error()
      .secure({ rolesAny: ['admin', 'editor'] })
      .render(MockComponent);

    expect(def.security).toEqual({
      authenticated: true,
      rolesAny: ['admin', 'editor'],
    });
  });
});

describe('isErrorDefinition', () => {
  it('returns true for valid ErrorDefinition', () => {
    const def = error().render(MockComponent);
    expect(isErrorDefinition(def)).toBe(true);
  });

  it('returns false for null', () => {
    expect(isErrorDefinition(null)).toBe(false);
  });

  it('returns false for page definition', () => {
    const def = page().render(MockComponent);
    expect(isErrorDefinition(def)).toBe(false);
  });
});

describe('notFound()', () => {
  it('renders a valid NotFoundDefinition', () => {
    const def = notFound().render(MockComponent);

    expect(isNotFoundDefinition(def)).toBe(true);
    expect(def.component).toBe(MockComponent);
    expect(def.middleware).toEqual([]);
  });

  it('chains middleware', () => {
    const def = notFound()
      .secure({ roles: ['admin'] })
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
  });

  it('surfaces a declarative security requirement after secure()', () => {
    const def = notFound().secure().render(MockComponent);

    expect(def.security).toEqual({ authenticated: true });
  });
});

describe('isNotFoundDefinition', () => {
  it('returns true for valid NotFoundDefinition', () => {
    const def = notFound().render(MockComponent);
    expect(isNotFoundDefinition(def)).toBe(true);
  });

  it('returns false for null', () => {
    expect(isNotFoundDefinition(null)).toBe(false);
  });

  it('returns false for layout definition', () => {
    const def = layout().render(MockComponent);
    expect(isNotFoundDefinition(def)).toBe(false);
  });
});
