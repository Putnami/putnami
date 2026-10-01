import { describe, expect, it } from 'bun:test';
import { createTooltipTimer } from '../../src/components/tooltip-timing';

/**
 * Exercises the Tooltip's show/hide scheduling without a DOM. The project's bun test env
 * renders with `renderToStaticMarkup` (no effects, no events), so the timer logic is
 * driven here through a manual fake clock injected into the controller.
 */

/** Minimal deterministic timer queue: `tick(ms)` fires everything due within the window. */
function fakeClock() {
  let now = 0;
  let nextId = 1;
  const scheduled = new Map<number, { fireAt: number; fn: () => void }>();

  return {
    setTimer(fn: () => void, ms: number) {
      const id = nextId++;
      scheduled.set(id, { fireAt: now + ms, fn });
      return id as unknown as ReturnType<typeof setTimeout>;
    },
    clearTimer(handle: ReturnType<typeof setTimeout>) {
      scheduled.delete(handle as unknown as number);
    },
    tick(ms: number) {
      now += ms;
      for (const [id, { fireAt, fn }] of [...scheduled.entries()]) {
        if (fireAt <= now) {
          scheduled.delete(id);
          fn();
        }
      }
    },
    get pending() {
      return scheduled.size;
    },
  };
}

function setup(delay = 200) {
  const clock = fakeClock();
  const events: string[] = [];
  const timer = createTooltipTimer({
    onShow: () => events.push('show'),
    onHide: () => events.push('hide'),
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  });
  return { clock, events, timer, delay };
}

describe('createTooltipTimer', () => {
  it('shows only after the full delay elapses', () => {
    const { clock, events, timer, delay } = setup();
    timer.show(delay, false);
    clock.tick(delay - 1);
    expect(events).toEqual([]);
    clock.tick(1);
    expect(events).toEqual(['show']);
  });

  it('does not schedule a show when disabled', () => {
    const { clock, events, timer, delay } = setup();
    timer.show(delay, true);
    expect(clock.pending).toBe(0);
    clock.tick(delay);
    expect(events).toEqual([]);
  });

  it('hide cancels a pending show so it never fires', () => {
    const { clock, events, timer, delay } = setup();
    timer.show(delay, false);
    timer.hide();
    expect(events).toEqual(['hide']);
    clock.tick(delay);
    expect(events).toEqual(['hide']);
    expect(clock.pending).toBe(0);
  });

  it('hide after the tooltip is shown still hides', () => {
    const { clock, events, timer, delay } = setup();
    timer.show(delay, false);
    clock.tick(delay);
    timer.hide();
    expect(events).toEqual(['show', 'hide']);
  });

  it('a fresh show replaces a pending one without leaking a timer', () => {
    const { clock, events, timer, delay } = setup();
    timer.show(delay, false);
    clock.tick(delay - 50);
    timer.show(delay, false);
    expect(clock.pending).toBe(1);
    // The first timer must not fire at its original deadline.
    clock.tick(50);
    expect(events).toEqual([]);
    // Only the second timer fires, a full delay after the second show.
    clock.tick(delay - 50);
    expect(events).toEqual(['show']);
  });

  it('cancel clears a pending show without hiding', () => {
    const { clock, events, timer, delay } = setup();
    timer.show(delay, false);
    timer.cancel();
    expect(clock.pending).toBe(0);
    clock.tick(delay);
    expect(events).toEqual([]);
  });
});
