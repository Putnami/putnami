import { describe, expect, it } from 'bun:test';
import { SignalLink } from '../../src/http/signal-link';

/** A signal that counts the abort listeners on it. */
function trackedSignal() {
  const controller = new AbortController();
  const signal = controller.signal;
  const active = new Set<unknown>();
  const add = signal.addEventListener.bind(signal);
  const remove = signal.removeEventListener.bind(signal);
  signal.addEventListener = ((type: string, listener: EventListenerOrEventListenerObject, options?: unknown) => {
    if (type === 'abort') active.add(listener);
    add(type, listener, options as AddEventListenerOptions);
  }) as typeof signal.addEventListener;
  signal.removeEventListener = ((type: string, listener: EventListenerOrEventListenerObject, options?: unknown) => {
    if (type === 'abort') active.delete(listener);
    remove(type, listener, options as EventListenerOptions);
  }) as typeof signal.removeEventListener;
  return { controller, signal, active };
}

describe('SignalLink', () => {
  it('aborts with the reason of the first source to abort and leaves the others', () => {
    const first = trackedSignal();
    const second = trackedSignal();
    const link = new SignalLink([first.signal, undefined, second.signal]);
    expect(link.signal.aborted).toBe(false);
    expect(first.active.size).toBe(1);
    expect(second.active.size).toBe(1);

    const reason = new DOMException('Request Timeout', 'TimeoutError');
    second.controller.abort(reason);

    expect(link.signal.aborted).toBe(true);
    expect(link.signal.reason).toBe(reason);
    expect(first.active.size).toBe(0);
    expect(second.active.size).toBe(0);
  });

  it('is aborted from the start when a source already aborted, and listens to none', () => {
    const live = trackedSignal();
    const link = new SignalLink([live.signal, AbortSignal.abort('gone')]);
    expect(link.signal.aborted).toBe(true);
    expect(link.signal.reason).toBe('gone');
    expect(live.active.size).toBe(0);
  });

  it('leaves its sources unaborted on release and no longer follows them', () => {
    const source = trackedSignal();
    const link = new SignalLink([source.signal]);
    link.release();
    expect(source.active.size).toBe(0);
    source.controller.abort();
    expect(link.signal.aborted).toBe(false);
  });
});
