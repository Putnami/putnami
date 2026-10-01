import { describe, expect, it } from 'bun:test';
import { loader } from '../../src/ssr/loader';
import { page } from '../../src/ssr/page';
import { layout } from '../../src/ssr/layout';

const MockComponent = () => null;

describe('page().cache()', () => {
  it('adds cache middleware to the page definition', () => {
    const def = page().cache({ maxAge: 3600 }).render(MockComponent);
    expect(def.middleware).toHaveLength(1);
    expect(typeof def.middleware[0]).toBe('function');
  });

  it('chains with other middleware', () => {
    const def = page().rateLimit({ max: 100 }).cache({ maxAge: 300 }).render(MockComponent);
    expect(def.middleware).toHaveLength(2);
  });

  it('accepts raw cacheControl string', () => {
    const def = page().cache({ cacheControl: 'private, no-cache' }).render(MockComponent);
    expect(def.middleware).toHaveLength(1);
  });

  it('accepts empty options', () => {
    const def = page().cache().render(MockComponent);
    expect(def.middleware).toHaveLength(1);
  });
});

describe('layout().cache()', () => {
  it('adds cache middleware to the layout definition', () => {
    const def = layout().cache({ maxAge: 600 }).render(MockComponent);
    expect(def.middleware).toHaveLength(1);
    expect(typeof def.middleware[0]).toBe('function');
  });

  it('chains with other middleware', () => {
    const def = layout()
      .secure({ roles: ['admin'] })
      .cache({ maxAge: 300, sMaxAge: 3600 })
      .render(MockComponent);
    expect(def.middleware).toHaveLength(2);
  });
});

describe('loader().cache()', () => {
  it('stores cache options in the loader definition', () => {
    const def = loader()
      .cache({ maxAge: 300, etag: true })
      .handle(() => ({}));

    expect(def.cache).toEqual({ maxAge: 300, etag: true });
  });

  it('stores cache with custom etag function', () => {
    const customEtag = (body: string) => `"v1-${body.length}"`;
    const def = loader()
      .cache({ etag: customEtag })
      .handle(() => ({}));

    expect(def.cache?.etag).toBe(customEtag);
  });

  it('does not include cache when not set', () => {
    const def = loader().handle(() => ({}));
    expect(def.cache).toBeUndefined();
  });

  it('chains with params and query', () => {
    const def = loader()
      .cache({ maxAge: 60, staleWhileRevalidate: 3600 })
      .params({ id: String })
      .query({ page: Number })
      .handle(() => ({}));

    expect(def.cache).toEqual({ maxAge: 60, staleWhileRevalidate: 3600 });
    expect(def.schemas?.params).toEqual({ id: String });
    expect(def.schemas?.query).toEqual({ page: Number });
  });

  it('simple mode loader has no cache field', () => {
    const def = loader(() => ({}));
    expect(def.cache).toBeUndefined();
  });
});
