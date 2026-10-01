import { afterEach, describe, expect, it } from 'bun:test';
import type { HttpRequestContext } from '../../src/http/http-context.type';
import type { HttpResponse } from '../../src/http/http-response';
import { type OAuthService, setActiveOAuthService } from '../../src/oauth/oauth.service';
import { authenticate } from '../../src/security/identity-resolver.middleware';
import { SecurityMiddleware } from '../../src/security/security.middleware';
import type { Principal } from '../../src/security/security.types';
import type { AuthStrategy } from '../../src/security/strategies';
import { bearerJwtStrategy } from '../../src/security/strategies/bearer-jwt.strategy';

function ctx(user?: Principal): HttpRequestContext {
  return { user, req: new Request('http://localhost/') } as HttpRequestContext;
}

async function run(middleware: ReturnType<typeof authenticate>, context: HttpRequestContext): Promise<boolean> {
  let passedThrough = false;
  await middleware(context, async () => {
    passedThrough = true;
    return undefined as unknown as HttpResponse;
  });
  return passedThrough;
}

const succeeds =
  (principal: Principal): AuthStrategy =>
  () =>
    principal;
const fails: AuthStrategy = () => undefined;
const throws: AuthStrategy = () => {
  throw new Error('strategy blew up');
};

describe('authenticate', () => {
  it('rejects an empty configuration', () => {
    expect(() => authenticate({})).toThrow(/at least one strategy/);
  });

  it('rejects combining anyOf and allOf', () => {
    expect(() => authenticate({ anyOf: [fails], allOf: [fails] })).toThrow(/either anyOf or allOf/);
  });

  it('never rejects a request — it always proceeds to next()', async () => {
    const context = ctx();
    expect(await run(authenticate({ anyOf: [fails] }), context)).toBe(true);
    expect(context.user).toBeUndefined();
  });

  describe('anyOf', () => {
    it('sets the principal of the first strategy that succeeds', async () => {
      const context = ctx();
      await run(authenticate({ anyOf: [fails, succeeds({ sub: 'second' }), succeeds({ sub: 'third' })] }), context);
      expect(context.user?.sub).toBe('second');
    });

    it('short-circuits: later strategies do not run once one succeeds', async () => {
      let laterRan = false;
      const later: AuthStrategy = () => {
        laterRan = true;
        return { sub: 'later' };
      };
      const context = ctx();
      await run(authenticate({ anyOf: [succeeds({ sub: 'first' }), later] }), context);
      expect(context.user?.sub).toBe('first');
      expect(laterRan).toBe(false);
    });

    it('treats a throwing strategy as "did not authenticate" and tries the next', async () => {
      const context = ctx();
      await run(authenticate({ anyOf: [throws, succeeds({ sub: 'recovered' })] }), context);
      expect(context.user?.sub).toBe('recovered');
    });

    it('leaves ctx.user unset when every strategy fails', async () => {
      const context = ctx();
      await run(authenticate({ anyOf: [fails, throws] }), context);
      expect(context.user).toBeUndefined();
    });
  });

  describe('allOf', () => {
    it('requires every strategy to succeed', async () => {
      const context = ctx();
      await run(authenticate({ allOf: [succeeds({ sub: 'a' }), fails] }), context);
      expect(context.user).toBeUndefined(); // one failure → no identity
    });

    it('merges principals in declaration order, last-write-wins on collision', async () => {
      const context = ctx();
      await run(
        authenticate({
          allOf: [
            succeeds({ sub: 'primary', scope: 'read', kind: 'apikey' }),
            succeeds({ sub: 'override', client_id: 'svc' }),
          ],
        }),
        context,
      );
      // `sub` collides → the later strategy wins; non-colliding claims are unioned.
      expect(context.user).toEqual({ sub: 'override', scope: 'read', kind: 'apikey', client_id: 'svc' });
    });

    it('is deterministic regardless of async completion order', async () => {
      // Strategy 0 resolves slowly, strategy 1 quickly; the merge must still be by
      // declaration order (strategy 1 wins the `sub` collision), not by which
      // promise settled first.
      const slow: AuthStrategy = async () => {
        await Bun.sleep(30);
        return { sub: 'slow-declared-first' };
      };
      const quick: AuthStrategy = async () => ({ sub: 'quick-declared-second' });
      const context = ctx();
      await run(authenticate({ allOf: [slow, quick] }), context);
      expect(context.user?.sub).toBe('quick-declared-second');
    });

    it('fails closed when a strategy throws', async () => {
      const context = ctx();
      await run(authenticate({ allOf: [succeeds({ sub: 'a' }), throws] }), context);
      expect(context.user).toBeUndefined();
    });
  });

  it('is first-win at the chain level: an already-authenticated request is not re-resolved', async () => {
    let ran = false;
    const strategy: AuthStrategy = () => {
      ran = true;
      return { sub: 'new' };
    };
    const context = ctx({ sub: 'existing' });
    await run(authenticate({ anyOf: [strategy] }), context);
    expect(context.user?.sub).toBe('existing');
    expect(ran).toBe(false);
  });
});

// End-to-end seam: a principal resolved by the framework's OWN bearer strategy
// must satisfy `.secure({ principalKind: 'user' })`. Before the kind-stamping
// fix, bearerJwtStrategy produced no `kind`, so this always 403'd.
describe('principalKind seam — real bearer strategy', () => {
  afterEach(() => setActiveOAuthService(undefined));

  function bearerContext(): HttpRequestContext {
    return {
      req: new Request('http://localhost/', { headers: { Authorization: 'Bearer good.jwt.token' } }),
      method: 'GET',
      path: () => '/',
    } as unknown as HttpRequestContext;
  }

  it('a bearer principal from authenticate() passes .secure({ principalKind: "user" })', async () => {
    setActiveOAuthService({
      verify: async () => ({ sub: 'svc-a', scope: 'ingest' }),
    } as unknown as OAuthService);

    const context = bearerContext();
    await run(authenticate({ anyOf: [bearerJwtStrategy()] }), context);
    expect(context.user?.kind).toBe('user');

    let allowed = false;
    const res = await SecurityMiddleware({ principalKind: 'user' })(context, async () => {
      allowed = true;
      return undefined as unknown as HttpResponse;
    });
    expect(allowed).toBe(true); // the handler ran…
    expect(res).toBeUndefined(); // …and no 403 was produced
  });
});
