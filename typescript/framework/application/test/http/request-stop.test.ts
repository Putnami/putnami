import { describe, expect, it } from 'bun:test';
import { isRequestStop } from '../../src/http/http-dispatcher';

function ended(reason?: unknown): AbortSignal {
  const controller = new AbortController();
  controller.abort(reason);
  return controller.signal;
}

describe('isRequestStop', () => {
  it('is false while the request still runs', () => {
    expect(isRequestStop(new DOMException('stop', 'AbortError'), new AbortController().signal)).toBe(false);
    expect(isRequestStop(new DOMException('stop', 'AbortError'), undefined)).toBe(false);
  });

  it('knows the signal reason and the stop errors, directly or as a cause', () => {
    const reason = new DOMException('Request Timeout', 'TimeoutError');
    const signal = ended(reason);
    expect(isRequestStop(reason, signal)).toBe(true);
    expect(isRequestStop(Object.assign(new Error('Query aborted'), { name: 'QueryAbortError' }), signal)).toBe(true);
    expect(isRequestStop(Object.assign(new Error('canceled'), { name: 'ClientCanceledError' }), signal)).toBe(true);
    expect(isRequestStop(new Error('wrapped', { cause: new DOMException('stop', 'AbortError') }), signal)).toBe(true);
  });

  it('keeps any other error a failure of its own', () => {
    const signal = ended(new DOMException('Request Timeout', 'TimeoutError'));
    expect(isRequestStop(new TypeError('broken'), signal)).toBe(false);
    expect(isRequestStop(new Error('wrapped', { cause: new TypeError('broken') }), signal)).toBe(false);
    expect(isRequestStop('a string', signal)).toBe(false);
  });
});
