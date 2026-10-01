import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto';
import { type InferConfig, useConfig, useContext, useLogger } from '@putnami/runtime';
import type { HttpRequestContext, HttpResponse } from '../http';
import { SessionConfig } from './session.config';

type CookieContext = HttpRequestContext & {
  cookies?: CookieStore;
  /** Response transform callbacks the cookie layer appends Set-Cookie to (framework-internal). */
  __callbacks?: ((r: HttpResponse) => HttpResponse)[];
};
type ResolvedSessionConfig = InferConfig<typeof SessionConfig>;
type LogWarnFn = (message: string, data?: Record<string, unknown>) => void;

/** Module-scope logger used when the request context has no logger attached. */
const fallbackLogger = (name: string) => useLogger(name);

class CookieStore {
  private _cookies: Record<string, string | null> = {};

  private _raw?: Record<string, string>;

  private _touched? = false;

  private readonly _ctxLogWarn: (loggerName: string, message: string, data?: Record<string, unknown>) => void;

  constructor(
    private readonly _config: ResolvedSessionConfig,
    private readonly _logWarn: LogWarnFn,
    ctxLogger: { named(n: string): { warn(m: unknown, ...p: unknown[]): void } } | undefined,
    raw?: Record<string, string>,
  ) {
    if (raw) {
      this._raw = { ...raw };
    }
    // Fall back to the module logger when the request context carries none:
    // cookie failures (decrypt errors, oversized Set-Cookie) must never be
    // silent: a swallowed warning here reads as an unexplainable login loop.
    this._ctxLogWarn = ctxLogger
      ? (loggerName, message, data) => ctxLogger.named(loggerName).warn(message, data)
      : (loggerName, message, data) => fallbackLogger(loggerName).warn(message, data);
  }

  all(): Record<string, string | null> {
    if (this._raw) {
      for (const name of Object.keys(this._raw)) {
        this._decryptOne(name);
      }
      this._raw = undefined;
    }
    return { ...this._cookies };
  }

  get(name: string): undefined | string {
    if (name in this._cookies) {
      return this._cookies[name] || undefined;
    }
    if (this._raw && name in this._raw) {
      this._decryptOne(name);
      return this._cookies[name] || undefined;
    }
    return undefined;
  }

  delete(name: string) {
    delete this._cookies[name];
    if (this._raw) delete this._raw[name];
  }

  set(name: string, value: string | null) {
    this._cookies[name] = value;
    if (this._raw) delete this._raw[name];
    this._touched = true;
  }

  get touched() {
    return this._touched;
  }

  warn(loggerName: string, message: string, data?: Record<string, unknown>) {
    this._ctxLogWarn(loggerName, message, data);
  }

  private _decryptOne(name: string) {
    if (!this._raw || !(name in this._raw)) return;
    const raw = this._raw[name];
    delete this._raw[name];
    if (raw) {
      const decrypted = decrypt(raw, this._config, this._logWarn);
      if (decrypted) {
        this._cookies[name] = decrypted;
      }
    }
  }
}

/** Returns the cookie store for the current request context, initializing it on first access. */
export function cookies() {
  const context = useContext<CookieContext>();
  if (context.cookies) {
    return context.cookies;
  }

  const config = { ...useConfig(SessionConfig) };
  if (!config.cookieSecret) {
    throw new Error('No secret is provided to cookie plugin');
  }
  const cookieString = context.req.headers.get('Cookie');
  if (!cookieString) {
    return initCookies(context, config);
  }

  // Bun.CookieMap is a spec-compliant native parser: it URI-decodes values
  // (so the percent-encoded ":" separators in our encrypted payloads round-trip)
  // and returns {} for empty/malformed headers without throwing.
  const data = new Bun.CookieMap(cookieString).toJSON();
  return initCookies(context, config, data);
}

/**
 * Browsers cap each cookie (name + "=" + value) at ~4096 bytes and DROP larger
 * ones silently — no error, no truncation, the Set-Cookie just never sticks.
 * Warn on the server side so an oversized session surfaces in logs instead of
 * as an unexplainable login loop.
 */
const MAX_COOKIE_PAIR_BYTES = 4093;

const setCookiesCallback = (store: CookieStore, config: ResolvedSessionConfig) => (_r: HttpResponse) => {
  let r = _r;
  if (store.touched) {
    const context = useContext<CookieContext>();

    for (const [name, value] of Object.entries(store.all())) {
      const host = context.host().split(':')[0];
      const isHttps = context.secured();

      const encrypted = encrypt(value, config);
      const pairBytes = name.length + 1 + encodeURIComponent(encrypted).length;
      if (pairBytes > MAX_COOKIE_PAIR_BYTES) {
        store.warn('cookies', 'Cookie exceeds the browser size limit and will be dropped', {
          name,
          size: pairBytes,
          limit: MAX_COOKIE_PAIR_BYTES,
        });
      }

      const serialized = serialize(name, encrypted, {
        path: '/',
        domain: host,
        secure: config.secure && isHttps,
        httpOnly: true,
        maxAge: config.ttl,
        priority: 'high',
        sameSite: config.sameSite,
      });
      r = r.pushHeader('Set-Cookie', serialized);
    }

    return r;
  }

  return r;
};

const initCookies = (
  context: CookieContext,
  config: ResolvedSessionConfig,
  values?: Record<string, string>,
): CookieStore => {
  const ctxLogger = context.logger;
  const logger = ctxLogger?.named('cookies');
  const logWarn: LogWarnFn = logger
    ? (message, data) => logger.warn(message, data)
    : (message, data) => fallbackLogger('cookies').warn(message, data);
  const cookies = new CookieStore(config, logWarn, ctxLogger, values);
  context.cookies = cookies;

  context.__callbacks ||= [];
  context.__callbacks.push(setCookiesCallback(cookies, config));

  return cookies;
};

// Current cookie payload format: "v2.<iv-b64url>.<authTag-b64url>.<ciphertext-b64url>".
// base64url costs 1.33x the raw bytes where the legacy hex format cost 2x —
// with the OAuth token set in a cookie session, hex overflowed the ~4096-byte
// browser cookie cap and the cookie was dropped silently.
// Every character (base64url alphabet + ".") survives encodeURIComponent
// unexpanded, so the serialized size is the payload size.
const V2_PREFIX = 'v2.';
const V2_IV_CHARS = 16; // 12 bytes, base64url without padding
const V2_AUTH_TAG_CHARS = 22; // 16 bytes, base64url without padding

function decrypt(data: string, config: ResolvedSessionConfig, logWarn: LogWarnFn) {
  try {
    if (!data || data.length === 0) {
      return undefined;
    }

    const key = getKey(config);

    let iv: Buffer;
    let authTag: Buffer;
    let ciphertext: Buffer;
    if (data.startsWith(V2_PREFIX)) {
      const parts = data.split('.');
      if (parts.length !== 4) {
        throw new Error('Invalid encrypted cookie format');
      }
      const [, ivB64, authTagB64, encryptedB64] = parts;
      if (ivB64.length !== V2_IV_CHARS || authTagB64.length !== V2_AUTH_TAG_CHARS || encryptedB64.length === 0) {
        throw new Error('Invalid encrypted cookie format');
      }
      iv = Buffer.from(ivB64, 'base64url');
      authTag = Buffer.from(authTagB64, 'base64url');
      ciphertext = Buffer.from(encryptedB64, 'base64url');
    } else {
      // Legacy format: "<iv-hex>:<authTag-hex>:<ciphertext-hex>". Still read so
      // sessions issued before the v2 rollout survive the upgrade; writes only
      // emit v2.
      const parts = data.split(':');
      if (parts.length !== 3) {
        throw new Error('Invalid encrypted cookie format');
      }
      const [ivHex, authTagHex, encryptedHex] = parts;
      if (ivHex.length !== 24 || authTagHex.length !== 32 || encryptedHex.length === 0) {
        throw new Error('Invalid encrypted cookie format');
      }
      iv = Buffer.from(ivHex, 'hex');
      authTag = Buffer.from(authTagHex, 'hex');
      ciphertext = Buffer.from(encryptedHex, 'hex');
    }

    const decipher = createDecipheriv(config.algorithm, key, iv) as ReturnType<typeof createDecipheriv> & {
      setAuthTag(tag: Buffer): void;
    };
    decipher.setAuthTag(authTag);
    let decrypted = decipher.update(ciphertext).toString('utf8');
    decrypted += decipher.final('utf8');

    return decrypted;
  } catch (e) {
    logWarn('Cookie decryption failed', { error: e instanceof Error ? e.message : String(e) });
    return undefined;
  }
}

function encrypt(_data: unknown, config: ResolvedSessionConfig) {
  let data = _data;
  if (!data) {
    return '';
  }
  if (typeof data !== 'string') {
    data = JSON.stringify(data);
  }
  const key = getKey(config);
  const iv = randomBytes(12);
  const cipher = createCipheriv(config.algorithm, key, iv) as ReturnType<typeof createCipheriv> & {
    getAuthTag(): Buffer;
  };
  const ciphertext = Buffer.concat([cipher.update(data as string, 'utf8'), cipher.final()]);
  const authTag = cipher.getAuthTag();

  return `${V2_PREFIX}${iv.toString('base64url')}.${authTag.toString('base64url')}.${ciphertext.toString('base64url')}`;
}

let _cachedKey: { secret: string; key: Buffer } | undefined;

/** Required key length, in bytes, for an `aes-<128|192|256>-...` cipher (e.g. 32 for aes-256-gcm). */
const requiredKeyBytes = (algorithm: string): number => {
  const match = /^aes-(128|192|256)-/i.exec(algorithm);
  if (!match) {
    throw new Error(`Unsupported session cipher '${algorithm}'. Expected an aes-128/192/256 algorithm.`);
  }
  return Number(match[1]) / 8;
};

const getKey = (config: ResolvedSessionConfig) => {
  if (_cachedKey && _cachedKey.secret === config.cookieSecret) {
    return _cachedKey.key;
  }
  // Validate the secret up front: `Buffer.from(secret, 'hex')` silently truncates
  // a non-hex or wrong-length value, which later surfaces as an opaque crypto
  // error on the first cookie write. Fail with an actionable message instead.
  const expectedBytes = requiredKeyBytes(config.algorithm);
  const isHex = config.cookieSecret.length === expectedBytes * 2 && /^[0-9a-f]+$/i.test(config.cookieSecret);
  const key = isHex ? Buffer.from(config.cookieSecret, 'hex') : Buffer.alloc(0);
  if (key.length !== expectedBytes) {
    throw new Error(
      `Invalid session cookieSecret for ${config.algorithm}: expected a ${expectedBytes * 2}-character hex ` +
        `string (${expectedBytes} bytes), received ${config.cookieSecret.length} characters. ` +
        `Generate one with: crypto.randomBytes(${expectedBytes}).toString('hex')`,
    );
  }
  _cachedKey = { secret: config.cookieSecret, key };
  return key;
};

type SerializeOptions = {
  path?: string;
  domain?: string;
  secure?: boolean;
  httpOnly?: boolean;
  maxAge?: number;
  priority?: 'low' | 'medium' | 'high';
  sameSite?: 'lax' | 'strict' | 'none';
};

const SAME_SITE_LABELS = { lax: 'Lax', strict: 'Strict', none: 'None' } as const;
const PRIORITY_LABELS = { low: 'Low', medium: 'Medium', high: 'High' } as const;

// Serialization stays hand-rolled: Bun.Cookie silently drops the `Priority`
// attribute (retested on Bun 1.4.0), which we set to `high` for session
// cookies. Parsing is delegated to Bun.CookieMap (see cookies() above).
function serialize(name: string, value: string, opts: SerializeOptions): string {
  let str = `${name}=${encodeURIComponent(value)}`;
  if (opts.maxAge !== undefined && Number.isFinite(opts.maxAge)) {
    str += `; Max-Age=${Math.floor(opts.maxAge)}`;
  }
  if (opts.domain) str += `; Domain=${opts.domain}`;
  if (opts.path) str += `; Path=${opts.path}`;
  if (opts.httpOnly) str += '; HttpOnly';
  if (opts.secure) str += '; Secure';
  if (opts.priority) str += `; Priority=${PRIORITY_LABELS[opts.priority]}`;
  if (opts.sameSite) str += `; SameSite=${SAME_SITE_LABELS[opts.sameSite]}`;
  return str;
}
