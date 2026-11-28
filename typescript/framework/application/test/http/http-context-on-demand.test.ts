import { describe, expect, it } from 'bun:test';
import { buildHttpContext, copyRequestContext, type RequestLifetime } from '../../src/http/http-context.builder';
import type { HttpRequestContextInternal } from '../../src/http/http-context.type';

type Reads = { url: number; method: number; signal: number };

/** A request that counts the reads of its native `url`, `method` and `signal` fields. */
function countingRequest(url: string, init?: RequestInit): { req: Request; reads: Reads } {
  const target = new Request(url, init);
  const reads = { url: 0, method: 0, signal: 0 };
  const req = new Proxy(target, {
    get(request, key) {
      if (key === 'url') reads.url++;
      if (key === 'method') reads.method++;
      if (key === 'signal') reads.signal++;
      const value = Reflect.get(request, key, request);
      return typeof value === 'function' ? value.bind(request) : value;
    },
  });
  return { req, reads };
}

const lifetime: RequestLifetime = {
  aborted: () => false,
  onAbort: () => {},
  offAbort: () => {},
  signal: () => new AbortController().signal,
  stop: () => {},
};

describe('the request fields a context reads', () => {
  it('takes the URL and the method its caller already read, without reading them again', () => {
    const { req, reads } = countingRequest('http://localhost/tasks/42?x=1');
    const ctx = buildHttpContext<HttpRequestContextInternal>({
      req,
      url: 'http://localhost/tasks/42?x=1',
      method: 'GET',
    });
    expect(ctx.url).toBe('http://localhost/tasks/42?x=1');
    expect(ctx.method).toBe('GET');
    expect(ctx.path()).toBe('tasks/42');
    expect(ctx.query()).toBe('?x=1');
    expect(ctx.queryParams()).toEqual({ x: '1' });
    expect(reads).toEqual({ url: 0, method: 0, signal: 0 });
  });

  it('reads the URL and the method of the request once each when its caller did not', () => {
    const { req, reads } = countingRequest('http://localhost/tasks?x=1', { method: 'POST' });
    const ctx = buildHttpContext<HttpRequestContextInternal>({ req });
    expect(ctx.url).toBe('http://localhost/tasks?x=1');
    expect(ctx.method).toBe('POST');
    expect(ctx.path()).toBe('tasks');
    expect(ctx.secured()).toBe(false);
    expect(reads).toEqual({ url: 1, method: 1, signal: 0 });
  });

  it('reads the signal of a request without a deadline on its first read, once', () => {
    const { req, reads } = countingRequest('http://localhost/a');
    const ctx = buildHttpContext<HttpRequestContextInternal>({ req });
    expect(reads.signal).toBe(0);
    const signal = ctx.signal;
    expect(signal).toBeInstanceOf(AbortSignal);
    expect(ctx.signal).toBe(signal);
    expect(reads.signal).toBe(1);
  });

  it('gives a request with a deadline the signal of its lifetime, never the request one', () => {
    const { req, reads } = countingRequest('http://localhost/a');
    const own = new AbortController().signal;
    const ctx = buildHttpContext<HttpRequestContextInternal>({ req, lifetime: { ...lifetime, signal: () => own } });
    expect(ctx.signal).toBe(own);
    expect(reads.signal).toBe(0);
  });

  it('keeps a rewritten URL apart from the request', () => {
    const ctx = buildHttpContext<HttpRequestContextInternal>({
      req: new Request('http://localhost/old?q=1'),
      url: 'http://localhost/new?q=1',
    });
    expect(ctx.url).toBe('http://localhost/new?q=1');
    expect(ctx.req.url).toBe('http://localhost/old?q=1');
    expect(ctx.path()).toBe('new');
    expect(ctx.method).toBe('GET');
  });
});

describe('the slots of a context', () => {
  const request = () => new Request('http://localhost/a');
  const signalBuilder = () => new AbortController().signal;
  function readByCaller(): HttpRequestContextInternal {
    return buildHttpContext({
      req: request(),
      url: 'http://localhost/a',
      method: 'GET',
      server: { upgrade: () => false },
      lifetime,
      maxBodySizeBytes: 1024,
    });
  }
  const builds: Record<string, () => HttpRequestContextInternal> = {
    'a request alone': () => buildHttpContext({ req: request() }),
    'a request with a deadline': () => buildHttpContext({ req: request(), lifetime }),
    'a request with a signal': () => buildHttpContext({ req: request(), signal: new AbortController().signal }),
    'a request with a signal builder': () => buildHttpContext({ req: request(), signal: signalBuilder }),
    'a request its caller read': readByCaller,
  };

  it('are the same own keys, in the same order, however the context is built', () => {
    const [first, ...others] = Object.values(builds).map((build) => Reflect.ownKeys(build()));
    for (const keys of others) expect(keys).toEqual(first);
  });

  it('take what the HTTP plugin and the dispatcher set on every request without a new own key', () => {
    const ctx = builds['a request with a deadline']();
    const before = Reflect.ownKeys(ctx);
    ctx.__drain = new AbortController().signal;
    ctx.deadlineAt = Date.now() + 1000;
    ctx.__matchedHandlers = [];
    ctx.route = '/a';
    ctx.params = {};
    ctx.statusCode = 200;
    ctx.__firstPartyErrors = false;
    ctx.__requestError = new Error('failed');
    ctx.__requestErrorLogged = true;
    ctx.__callbacks = [];
    ctx.user = { sub: 'u' };
    expect(Reflect.ownKeys(ctx)).toEqual(before);
  });

  it('start empty, and a context copy keeps what the plugin set', () => {
    const ctx = builds['a request alone']();
    expect(ctx.deadlineAt).toBeUndefined();
    expect(ctx.__drain).toBeUndefined();
    expect(ctx.__firstPartyErrors).toBeUndefined();
    expect(ctx.__requestErrorLogged).toBe(false);
    const drain = new AbortController().signal;
    ctx.__drain = drain;
    ctx.deadlineAt = 42;
    const copy = copyRequestContext(ctx) as HttpRequestContextInternal;
    expect(copy.__drain).toBe(drain);
    expect(copy.deadlineAt).toBe(42);
    expect(copy.path()).toBe('a');
  });
});
