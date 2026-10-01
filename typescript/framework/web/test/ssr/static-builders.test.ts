import { describe, expect, it } from 'bun:test';
import { loader } from '../../src/ssr/loader';
import { page } from '../../src/ssr/page';

const MockComponent = () => null;

describe('page().static()', () => {
  it('marks a page as SSG with no options', () => {
    const def = page().static().render(MockComponent);
    expect(def.static).toEqual({ mode: 'ssg' });
  });

  it('marks a page as ISR with a revalidate number', () => {
    const def = page().static({ revalidate: 60 }).render(MockComponent);
    expect(def.static).toEqual({ mode: 'isr', revalidate: { seconds: 60 } });
  });

  it('marks a page as ISR with tags', () => {
    const def = page()
      .static({ revalidate: { tags: ['posts'] } })
      .render(MockComponent);
    expect(def.static).toEqual({ mode: 'isr', revalidate: { tags: ['posts'] } });
  });

  it('keeps the paths function for dynamic routes', () => {
    const paths = async () => [{ id: '1' }, { id: '2' }];
    const def = page().static({ paths }).render(MockComponent);
    expect(def.static?.paths).toBe(paths);
  });

  it('composes with other builder methods', () => {
    const def = page().status(200).static().render(MockComponent);
    expect(def.static).toEqual({ mode: 'ssg' });
    expect(def.statusCode).toBe(200);
  });

  it('omits static when never called', () => {
    const def = page().render(MockComponent);
    expect(def.static).toBeUndefined();
  });
});

describe('loader().static()', () => {
  it('marks a loader as build-time', () => {
    const def = loader()
      .static()
      .handle(() => ({ ok: true }));
    expect(def.static).toBe(true);
  });

  it('omits static when never called', () => {
    const def = loader().handle(() => ({ ok: true }));
    expect(def.static).toBeUndefined();
  });

  it('composes with params', () => {
    const def = loader()
      .static()
      .params({ id: String })
      .handle(() => ({ ok: true }));
    expect(def.static).toBe(true);
    expect(def.schemas?.params).toBeDefined();
  });
});
