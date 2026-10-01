import { createCipheriv, randomBytes as legacyRandomBytes } from 'node:crypto';
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader, resetLoggerConfig, runInContext, useConfig } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { type HttpRequestContext, HttpResponse } from '../../src/http';
import { UrlScanner } from '../../src/http/url.scanner';
import { CookieSessionStore } from '../../src/session/cookie.store';
import { OAUTH_PENDING_SESSION_LIMIT } from '../../src/oauth/oauth-session';
import { cookies } from '../../src/session/cookies';
import { SessionConfig } from '../../src/session/session.config';

type TestCookieContext = HttpRequestContext & {
  cookies?: unknown;
  logger: MemoryLogger;
};

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;
const PRIMARY_SECRET = 'a'.repeat(64);
const ROTATED_SECRET = 'b'.repeat(64);

function setSessionConfig(
  overrides: Partial<{
    algorithm: string;
    cookieName: string;
    cookieSecret: string;
    sameSite: 'lax' | 'strict' | 'none';
    secure: boolean;
    ttl: number;
  }> = {},
): void {
  process.env.CONFIG_DATA = JSON.stringify({
    session: {
      algorithm: 'aes-256-gcm',
      cookieName: 'session',
      cookieSecret: PRIMARY_SECRET,
      sameSite: 'strict',
      secure: true,
      ttl: 300,
      ...overrides,
    },
  });
  resetConfigLoader();
}

function createContext({
  cookie,
  host = 'app.example.com:8443',
  logger = new MemoryLogger(),
  secure = true,
}: {
  cookie?: string;
  host?: string;
  logger?: MemoryLogger;
  secure?: boolean;
} = {}): TestCookieContext {
  const protocol = secure ? 'https' : 'http';
  const headers = new Headers(cookie ? { Cookie: cookie } : undefined);
  const req = new Request(`${protocol}://${host}/profile`, { headers });

  return {
    body: async () => undefined,
    domain: () => host.split(':')[0],
    headers: req.headers,
    host: () => host,
    logger,
    method: 'GET',
    path: () => '/profile',
    query: () => '',
    queryParams: () => ({}),
    req,
    secured: () => secure,
    throw: (status: number, message?: string) => {
      throw new Error(`Unexpected throw ${status}${message ? `: ${message}` : ''}`);
    },
    url: req.url,
  } as TestCookieContext;
}

function applyCallbacks(context: TestCookieContext): HttpResponse {
  return (context.__callbacks ?? []).reduce((response, callback) => callback(response), new HttpResponse('ok'));
}

function extractCookiePair(setCookieHeader: string): string {
  const [cookiePair] = setCookieHeader.split(';');
  return cookiePair;
}

function tamperCookie(cookiePair: string): string {
  const [name, encodedValue] = cookiePair.split('=');
  const value = decodeURIComponent(encodedValue);
  // Corrupt a decoded ciphertext byte instead of flipping the last base64url
  // character. The final char of a base64url string only carries the high bits
  // of the trailing byte — its low bits are discarded on decode — so a bare
  // character flip is a no-op ~25% of the time (whenever the byte already ends
  // in those bits), leaving the cookie byte-identical, still authentic, and
  // decryptable. Round-tripping through bytes guarantees the ciphertext (and so
  // the GCM auth-tag check) actually changes. Payload: "v2.<iv>.<tag>.<ciphertext>".
  const parts = value.split('.');
  const ciphertext = Buffer.from(parts[parts.length - 1], 'base64url');
  ciphertext[0] ^= 0xff;
  parts[parts.length - 1] = ciphertext.toString('base64url');
  return `${name}=${encodeURIComponent(parts.join('.'))}`;
}

function issueSessionCookie(session: Record<string, unknown>): string {
  const context = createContext();

  return runInContext(context, () => {
    const store = new CookieSessionStore();
    store.setAll('__cookie__', session);

    const response = applyCallbacks(context);
    const setCookieHeader = response.getHeaders('Set-Cookie')[0];
    if (!setCookieHeader) {
      throw new Error('Expected a Set-Cookie header');
    }

    return extractCookiePair(setCookieHeader);
  }) as string;
}

beforeEach(() => {
  delete process.env.CONFIG_DATA;
  resetConfigLoader();
  resetLoggerConfig();
});

afterEach(() => {
  if (ORIGINAL_CONFIG_DATA === undefined) {
    delete process.env.CONFIG_DATA;
  } else {
    process.env.CONFIG_DATA = ORIGINAL_CONFIG_DATA;
  }
  resetConfigLoader();
});

describe('SessionConfig', () => {
  it('applies the default session settings', () => {
    process.env.CONFIG_DATA = JSON.stringify({
      session: {
        cookieSecret: PRIMARY_SECRET,
      },
    });
    resetConfigLoader();

    const config = useConfig(SessionConfig);

    expect(config.store).toBe('cookie');
    expect(config.cookieName).toBe('session');
    expect(config.sameSite).toBe('lax');
    expect(config.secure).toBe(true);
    expect(config.algorithm).toBe('aes-256-gcm');
    expect(config.ttl).toBe(60 * 60 * 24 * 7);
  });
});

describe('cookies()', () => {
  it('encrypts values into Set-Cookie headers and decrypts them on the next request', () => {
    setSessionConfig({ sameSite: 'strict', ttl: 300 });
    const context = createContext();

    const { cookiePair, setCookieHeader } = runInContext(context, () => {
      cookies().set('session', JSON.stringify({ userId: 42 }));

      const response = applyCallbacks(context);
      const header = response.getHeaders('Set-Cookie')[0];
      if (!header) {
        throw new Error('Expected a Set-Cookie header');
      }

      return {
        cookiePair: extractCookiePair(header),
        setCookieHeader: header,
      };
    }) as { cookiePair: string; setCookieHeader: string };

    expect(setCookieHeader).toContain('session=');
    expect(setCookieHeader).toContain('Domain=app.example.com');
    expect(setCookieHeader).toContain('Path=/');
    expect(setCookieHeader).toContain('HttpOnly');
    expect(setCookieHeader).toContain('Secure');
    expect(setCookieHeader).toContain('Max-Age=300');
    expect(setCookieHeader).toContain('SameSite=Strict');

    const nextContext = createContext({ cookie: cookiePair });
    runInContext(nextContext, () => {
      expect(cookies().get('session')).toBe(JSON.stringify({ userId: 42 }));
      expect(cookies().all()).toEqual({
        session: JSON.stringify({ userId: 42 }),
      });
    });
  });

  it('reads the encrypted session cookie when the header carries other cookies', () => {
    setSessionConfig({ sameSite: 'strict', ttl: 300 });
    const context = createContext();

    const cookiePair = runInContext(context, () => {
      cookies().set('session', JSON.stringify({ userId: 7 }));
      const response = applyCallbacks(context);
      const header = response.getHeaders('Set-Cookie')[0];
      if (!header) {
        throw new Error('Expected a Set-Cookie header');
      }
      return extractCookiePair(header);
    }) as string;

    // The encrypted payload shares the header with unrelated cookies —
    // Bun.CookieMap must isolate (and, for the legacy hex format whose ":"
    // separators percent-encode to %3A, URI-decode) the session entry for the
    // round-trip to succeed.
    const nextContext = createContext({ cookie: `theme=dark; ${cookiePair}; locale=en` });
    runInContext(nextContext, () => {
      expect(cookies().get('session')).toBe(JSON.stringify({ userId: 7 }));
    });
  });

  it('discards forged non-encrypted cookie values instead of falling back to raw', () => {
    setSessionConfig();
    const forgedPayload = JSON.stringify({ userId: 999 });
    const context = createContext({ cookie: `session=${encodeURIComponent(forgedPayload)}` });

    runInContext(context, () => {
      const store = cookies();
      expect(store.get('session')).toBeUndefined();
      expect(store.all()).toEqual({});
    });
  });

  it('does not append Set-Cookie headers when the store is untouched', () => {
    setSessionConfig();
    const context = createContext();

    runInContext(context, () => {
      cookies();
      const response = applyCallbacks(context);
      expect(response.getHeaders('Set-Cookie')).toEqual([]);
    });
  });

  it('rejects a wrong-length cookieSecret with an actionable error instead of an opaque crypto failure', () => {
    setSessionConfig({ cookieSecret: 'not-a-valid-hex-key' });
    const context = createContext();

    expect(() =>
      runInContext(context, () => {
        cookies().set('session', JSON.stringify({ userId: 1 }));
        applyCallbacks(context);
      }),
    ).toThrow(/64-character hex string \(32 bytes\)/);
  });

  it('keeps the Secure flag behind a TLS-terminating proxy (http URL + x-forwarded-proto)', () => {
    setSessionConfig({ secure: true });
    // Simulate Bun receiving plaintext http from a proxy that terminated TLS.
    const headers = new Headers({ 'x-forwarded-proto': 'https' });
    const req = new Request('http://app.example.com/profile', { headers });
    const urlScanner = new UrlScanner(req.url, req.headers);
    const context = {
      ...createContext(),
      headers: req.headers,
      req,
      url: req.url,
      // Wire secured() to the real scanner so the proxy header is honoured.
      secured: () => urlScanner.secured,
    } as TestCookieContext;
    context.__callbacks = [];

    runInContext(context, () => {
      cookies().set('session', JSON.stringify({ userId: 1 }));
      const response = applyCallbacks(context);
      const header = response.getHeaders('Set-Cookie')[0] ?? '';
      expect(header).toContain('Secure');
    });
  });
});

describe('CookieSessionStore', () => {
  it('returns an empty session for tampered ciphertext and logs the failure', () => {
    setSessionConfig();
    const cookiePair = issueSessionCookie({ role: 'admin' });

    const logger = new MemoryLogger();
    const context = createContext({
      cookie: tamperCookie(cookiePair),
      logger,
    });

    runInContext(context, () => {
      const store = new CookieSessionStore();
      expect(store.getAll('__cookie__')).toEqual({});
    });

    expect(
      logger.entries.some((entry) => entry.logger === 'cookies' && entry.message === 'Cookie decryption failed'),
    ).toBe(true);
  });

  it('returns an empty session when the cookie was encrypted with a different key', () => {
    setSessionConfig({ cookieSecret: PRIMARY_SECRET });
    const cookiePair = issueSessionCookie({ userId: 7 });

    setSessionConfig({ cookieSecret: ROTATED_SECRET });
    const logger = new MemoryLogger();
    const context = createContext({ cookie: cookiePair, logger });

    runInContext(context, () => {
      const store = new CookieSessionStore();
      expect(store.getAll('__cookie__')).toEqual({});
    });

    expect(
      logger.entries.some((entry) => entry.logger === 'cookies' && entry.message === 'Cookie decryption failed'),
    ).toBe(true);
  });

  it('supports set, get, delete, and clearing the cookie-backed session', () => {
    setSessionConfig();
    const initialContext = createContext();

    const issuedCookie = runInContext(initialContext, () => {
      const store = new CookieSessionStore();
      store.set('__cookie__', 'userId', 42);
      store.set('__cookie__', 'theme', 'dark');

      expect(store.get<number>('__cookie__', 'userId')).toBe(42);
      expect(store.getAll('__cookie__')).toEqual({
        theme: 'dark',
        userId: 42,
      });

      const response = applyCallbacks(initialContext);
      const setCookieHeader = response.getHeaders('Set-Cookie')[0];
      if (!setCookieHeader) {
        throw new Error('Expected a Set-Cookie header');
      }

      return extractCookiePair(setCookieHeader);
    }) as string;

    const updateContext = createContext({ cookie: issuedCookie });
    const clearedCookie = runInContext(updateContext, () => {
      const store = new CookieSessionStore();

      expect(store.exists('__cookie__')).toBe(true);
      expect(store.getAll('__cookie__')).toEqual({
        theme: 'dark',
        userId: 42,
      });

      store.delete('__cookie__', 'theme');
      expect(store.getAll('__cookie__')).toEqual({
        userId: 42,
      });

      store.deleteAll('__cookie__');
      expect(store.getAll('__cookie__')).toEqual({});

      const response = applyCallbacks(updateContext);
      const setCookieHeader = response.getHeaders('Set-Cookie')[0];
      if (!setCookieHeader) {
        throw new Error('Expected a Set-Cookie header');
      }

      return extractCookiePair(setCookieHeader);
    }) as string;

    const finalContext = createContext({ cookie: clearedCookie });
    runInContext(finalContext, () => {
      const store = new CookieSessionStore();
      expect(store.getAll('__cookie__')).toEqual({});
      expect(store.exists('__cookie__')).toBe(false);
    });
  });
});

describe('cookie payload format', () => {
  it('emits the v2 base64url format and survives encodeURIComponent unexpanded', () => {
    setSessionConfig();
    const cookiePair = issueSessionCookie({ userId: 42 });

    const [, encodedValue] = cookiePair.split('=');
    expect(decodeURIComponent(encodedValue).startsWith('v2.')).toBe(true);
    // Every character of the v2 alphabet (base64url + ".") is URI-safe, so the
    // on-the-wire size equals the payload size — no percent-encoding blowup.
    expect(encodedValue).toBe(decodeURIComponent(encodedValue));
  });

  it('still decrypts legacy hex-format cookies issued before the v2 rollout', () => {
    setSessionConfig();
    const session = JSON.stringify({ userId: 7, role: 'admin' });

    const key = Buffer.from(PRIMARY_SECRET, 'hex');
    const iv = legacyRandomBytes(12);
    const cipher = createCipheriv('aes-256-gcm', key, iv) as ReturnType<typeof createCipheriv> & {
      getAuthTag(): Buffer;
    };
    let encrypted = cipher.update(session, 'utf8', 'hex');
    encrypted += cipher.final('hex');
    const legacyValue = `${iv.toString('hex')}:${cipher.getAuthTag().toString('hex')}:${encrypted}`;

    const context = createContext({ cookie: `session=${encodeURIComponent(legacyValue)}` });
    runInContext(context, () => {
      expect(cookies().get('session')).toBe(session);
    });
  });

  it('keeps a real-size OAuth token session under the browser cookie cap', () => {
    setSessionConfig();
    // Mirrors what the OAuth client stores after login, at production token
    // sizes: a ~1KB access JWT, a ~800B id JWT, and a FULL pending-login list
    // at OAUTH_PENDING_SESSION_LIMIT. This is the worst-case post-login
    // Set-Cookie — the limit and the cookie codec must together keep it under
    // the ~4093-byte browser cap, or login loops unrecoverably (the legacy
    // hex format put the single-pending case at ~4.4KB; the old limit of 8
    // put even the base64url format at ~4.9KB).
    const pending = {
      state: 's'.repeat(36),
      codeVerifier: 'v'.repeat(43),
      nonce: 'n'.repeat(36),
      redirectTo: '/',
      redirectUri: 'https://app.example.com/auth/callback',
    };
    const cookiePair = issueSessionCookie({
      oauth: {
        accessToken: 'a'.repeat(1000),
        refreshToken: 'r'.repeat(36),
        idToken: 'i'.repeat(800),
        expireAt: 1_783_600_000_000,
      },
      oauth_pending: Array.from({ length: OAUTH_PENDING_SESSION_LIMIT }, () => ({ ...pending })),
    });

    expect(cookiePair.length).toBeLessThanOrEqual(4093);
  });

  it('warns when a serialized cookie exceeds the browser size limit', () => {
    setSessionConfig();
    const logger = new MemoryLogger();
    const context = createContext({ logger });

    runInContext(context, () => {
      cookies().set('session', 'x'.repeat(4000));
      applyCallbacks(context);
    });

    const warning = logger.entries.find(
      (entry) =>
        entry.logger === 'cookies' && entry.message === 'Cookie exceeds the browser size limit and will be dropped',
    );
    expect(warning).toBeDefined();
  });
});
