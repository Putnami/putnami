import { describe, expect, it } from 'bun:test';
import { error, layout, notFound, page } from '../../src/client/page';

describe('client page builders', () => {
  const Component = () => null;

  it('captures authentication-only requirements by default', () => {
    const definition = page().secure().render(Component);

    expect(definition).toEqual({
      component: Component,
      security: { authenticated: true },
    });
  });

  it('captures declarative role and scope requirements', () => {
    const definition = layout()
      .secure({
        roles: ['admin'],
        rolesAny: ['editor'],
        scopes: ['docs:read'],
        scopesAny: ['docs:write'],
      })
      .render(Component);

    expect(definition).toEqual({
      component: Component,
      security: {
        authenticated: true,
        roles: ['admin'],
        rolesAny: ['editor'],
        scopes: ['docs:read'],
        scopesAny: ['docs:write'],
      },
    });
  });

  it('marks function guards as indeterminate so client gating default-denies', () => {
    const definition = error()
      .secure(() => true)
      .render(Component);

    expect(definition.security).toEqual({ authenticated: true, indeterminate: true });
  });

  it('keeps server-only builder helpers as chainable no-ops on the client', () => {
    const definition = notFound()
      .cors({ origin: 'https://example.com' })
      .rateLimit({ max: 100 })
      .cache({ maxAge: 60 })
      .use(() => undefined)
      .status(404)
      .render(Component);

    expect(definition).toEqual({ component: Component });
  });
});
