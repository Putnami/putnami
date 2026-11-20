import { describe, expect, it } from 'bun:test';
import { isRequestStop, requestEnded } from '../../src/http/http-dispatcher';

function ended(reason?: unknown): { signal: AbortSignal } {
  const controller = new AbortController();
  controller.abort(reason);
  return { signal: controller.signal };
}

const named = (name: string) => Object.assign(new Error(name), { name });

describe('isRequestStop', () => {
  it('is false while the request still runs', () => {
    const running = { signal: new AbortController().signal, deadlineAt: Date.now() + 60_000 };
    expect(requestEnded(running)).toBe(false);
    expect(isRequestStop(new DOMException('stop', 'AbortError'), running)).toBe(false);
    expect(isRequestStop(new DOMException('stop', 'AbortError'), undefined)).toBe(false);
  });

  it('knows the signal reason and the stop errors, directly or as a cause', () => {
    const reason = new DOMException('Request Timeout', 'TimeoutError');
    const request = ended(reason);
    expect(isRequestStop(reason, request)).toBe(true);
    expect(isRequestStop(named('QueryAbortError'), request)).toBe(true);
    expect(isRequestStop(named('ClientCanceledError'), request)).toBe(true);
    expect(isRequestStop(named('ClientDeadlineError'), request)).toBe(true);
    expect(isRequestStop(new Error('wrapped', { cause: new DOMException('stop', 'AbortError') }), request)).toBe(true);
  });

  it('ends the request at its deadline, before the timeout aborts the signal', () => {
    const atDeadline = { signal: new AbortController().signal, deadlineAt: Date.now() - 1 };
    expect(requestEnded(atDeadline)).toBe(true);
    expect(isRequestStop(named('ClientDeadlineError'), atDeadline)).toBe(true);
  });

  it('counts a client deadline as a stop only when the request ended by its own deadline', () => {
    const left = ended(new DOMException('The client left', 'AbortError'));
    expect(isRequestStop(named('ClientDeadlineError'), left)).toBe(false);
  });

  it('keeps any other error a failure of its own', () => {
    const request = ended(new DOMException('Request Timeout', 'TimeoutError'));
    expect(isRequestStop(new TypeError('broken'), request)).toBe(false);
    expect(isRequestStop(new Error('wrapped', { cause: new TypeError('broken') }), request)).toBe(false);
    expect(isRequestStop('a string', request)).toBe(false);
  });
});
