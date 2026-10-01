import { describe, expect, it, mock } from 'bun:test';
import { ErrorBuilder, error, isErrorDefinition } from '../../src/ssr/error';

const MockComponent = () => null;

describe('error()', () => {
  it('returns an ErrorBuilder', () => {
    expect(error()).toBeInstanceOf(ErrorBuilder);
  });
});

describe('ErrorBuilder', () => {
  it('renders a valid ErrorDefinition', () => {
    const def = error().render(MockComponent);

    expect(isErrorDefinition(def)).toBe(true);
    expect(def.component).toBe(MockComponent);
    expect(def.middleware).toEqual([]);
    expect(def.statusCode).toBeUndefined();
  });

  it('chains secure() and produces middleware', () => {
    const def = error()
      .secure({ roles: ['admin'] })
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(typeof def.middleware[0]).toBe('function');
  });

  it('chains rateLimit() and produces middleware', () => {
    const def = error().rateLimit({ max: 100 }).render(MockComponent);

    expect(def.middleware).toHaveLength(1);
  });

  it('chains multiple middleware calls', () => {
    const def = error()
      .secure({ roles: ['user'] })
      .rateLimit({ max: 50 })
      .render(MockComponent);

    expect(def.middleware).toHaveLength(2);
  });

  it('sets status code via status()', () => {
    const def = error().status(500).render(MockComponent);

    expect(def.statusCode).toBe(500);
  });

  it('accepts custom middleware via use()', () => {
    const customMiddleware = mock(async (_ctx: unknown, next: () => Promise<unknown>) => next());
    const def = error()
      .use(customMiddleware as never)
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(def.middleware[0]).toBe(customMiddleware);
  });

  it('returns a new middleware array (not shared reference)', () => {
    const builder = error().secure();
    const def1 = builder.render(MockComponent);
    const def2 = builder.render(MockComponent);

    expect(def1.middleware).not.toBe(def2.middleware);
    expect(def1.middleware).toEqual(def2.middleware);
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

  it('returns false for undefined', () => {
    expect(isErrorDefinition(undefined)).toBe(false);
  });

  it('returns false for plain object', () => {
    expect(isErrorDefinition({ middleware: [] })).toBe(false);
  });

  it('returns false for page definition', () => {
    expect(isErrorDefinition({ __page: 'putnami:page' })).toBe(false);
  });
});
