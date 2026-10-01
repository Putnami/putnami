/**
 * The controller isolates the scheduling decisions (isDisabled suppresses
 * show, a fresh show replaces a pending one, hide and unmount cancel) over
 * injectable timer functions so they are unit-testable with a fake clock.
 */

type TimerHandle = ReturnType<typeof setTimeout>;

/** Options for {@link createTooltipTimer}. Timer functions are injectable for testing. */
interface TooltipTimerOptions {
  /** Invoked when the show delay elapses without an intervening hide/cancel. */
  onShow: () => void;
  /** Invoked synchronously on hide. */
  onHide: () => void;
  /** Schedules `fn` after `ms`; defaults to the global `setTimeout`. */
  setTimer?: (fn: () => void, ms: number) => TimerHandle;
  /** Cancels a scheduled timer; defaults to the global `clearTimeout`. */
  clearTimer?: (handle: TimerHandle) => void;
}

/** Imperative tooltip timer controlling a single pending show. */
export interface TooltipTimer {
  /** Schedule a show after `delay` ms, unless `isDisabled`. Replaces any pending show. */
  show(delay: number, isDisabled: boolean): void;
  /** Cancel any pending show and hide immediately. */
  hide(): void;
  /** Cancel any pending show without hiding (unmount cleanup). */
  cancel(): void;
}

/** Creates a {@link TooltipTimer}. State (the pending handle) is closed over, not exposed. */
export function createTooltipTimer({
  onShow,
  onHide,
  setTimer = setTimeout,
  clearTimer = clearTimeout,
}: TooltipTimerOptions): TooltipTimer {
  let handle: TimerHandle | null = null;

  const cancel = () => {
    if (handle !== null) {
      clearTimer(handle);
      handle = null;
    }
  };

  return {
    show(delay, isDisabled) {
      if (isDisabled) return;
      cancel();
      handle = setTimer(() => {
        handle = null;
        onShow();
      }, delay);
    },
    hide() {
      cancel();
      onHide();
    },
    cancel,
  };
}
