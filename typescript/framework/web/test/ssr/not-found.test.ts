import { describe, expect, it, mock } from 'bun:test';
import { NotFoundBuilder, isNotFoundDefinition, notFound } from '../../src/ssr/not-found';

const MockComponent = () => null;

describe('notFound()', () => {
  it('returns a NotFoundBuilder', () => {
    expect(notFound()).toBeInstanceOf(NotFoundBuilder);
  });
});

describe('NotFoundBuilder', () => {
  it('renders a valid NotFoundDefinition', () => {
    const def = notFound().render(MockComponent);

    expect(isNotFoundDefinition(def)).toBe(true);
    expect(def.component).toBe(MockComponent);
    expect(def.middleware).toEqual([]);
  });

  it('chains secure() and produces middleware', () => {
    const def = notFound()
      .secure({ roles: ['admin'] })
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(typeof def.middleware[0]).toBe('function');
  });

  it('chains rateLimit() and produces middleware', () => {
    const def = notFound().rateLimit({ max: 50 }).render(MockComponent);

    expect(def.middleware).toHaveLength(1);
  });

  it('chains multiple middleware calls', () => {
    const def = notFound().secure().rateLimit({ max: 20 }).render(MockComponent);

    expect(def.middleware).toHaveLength(2);
  });

  it('accepts custom middleware via use()', () => {
    const customMiddleware = mock(async (_ctx: unknown, next: () => Promise<unknown>) => next());
    const def = notFound()
      .use(customMiddleware as never)
      .render(MockComponent);

    expect(def.middleware).toHaveLength(1);
    expect(def.middleware[0]).toBe(customMiddleware);
  });

  it('returns a new middleware array (not shared reference)', () => {
    const builder = notFound().rateLimit({ max: 10 });
    const def1 = builder.render(MockComponent);
    const def2 = builder.render(MockComponent);

    expect(def1.middleware).not.toBe(def2.middleware);
    expect(def1.middleware).toEqual(def2.middleware);
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

  it('returns false for undefined', () => {
    expect(isNotFoundDefinition(undefined)).toBe(false);
  });

  it('returns false for plain object', () => {
    expect(isNotFoundDefinition({ middleware: [] })).toBe(false);
  });

  it('returns false for page definition', () => {
    expect(isNotFoundDefinition({ __page: 'putnami:page' })).toBe(false);
  });
});
