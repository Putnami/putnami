import { describe, expect, it, mock } from 'bun:test';
import { isLayoutDefinition, LayoutBuilder, layout } from '../../src/ssr/layout';

const MockComponent = () => null;

describe('layout()', () => {
  it('returns a LayoutBuilder', () => {
    expect(layout()).toBeInstanceOf(LayoutBuilder);
  });
});

describe('LayoutBuilder', () => {
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

  it('chains rateLimit() and produces middleware', () => {
    const def = layout().rateLimit({ max: 100 }).render(MockComponent);

    expect(def.middleware).toHaveLength(1);
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

  it('returns a new middleware array (not shared reference)', () => {
    const builder = layout().secure();
    const def1 = builder.render(MockComponent);
    const def2 = builder.render(MockComponent);

    expect(def1.middleware).not.toBe(def2.middleware);
    expect(def1.middleware).toEqual(def2.middleware);
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

  it('returns false for undefined', () => {
    expect(isLayoutDefinition(undefined)).toBe(false);
  });

  it('returns false for plain object', () => {
    expect(isLayoutDefinition({ middleware: [] })).toBe(false);
  });

  it('returns false for PageDefinition', () => {
    // A page definition has __page, not __layout
    expect(isLayoutDefinition({ __page: 'putnami:page', middleware: [] })).toBe(false);
  });
});
