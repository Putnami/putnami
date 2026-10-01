import { describe, expect, it } from 'bun:test';
import { CircuitBreaker, DEFAULT_BREAKER_OPEN_DURATION_MS } from '../../src/security/introspect-breaker';

// TypeScript twin of go/framework/security/introspect_breaker_test.go, white-box
// over the breaker state machine. A controllable clock replaces Go's white-box
// poke of `openedAt` so the open window can elapse deterministically without a
// real sleep.

function clock(start = 1000): { now: () => number; advance: (ms: number) => void } {
  let t = start;
  return {
    now: () => t,
    advance: (ms) => {
      t += ms;
    },
  };
}

describe('CircuitBreaker', () => {
  it('is disabled (undefined) for a zero or negative threshold', () => {
    expect(CircuitBreaker.create({})).toBeUndefined();
    expect(CircuitBreaker.create({ failureThreshold: 0 })).toBeUndefined();
    expect(CircuitBreaker.create({ failureThreshold: -1 })).toBeUndefined();
    expect(CircuitBreaker.create(undefined)).toBeUndefined();
  });

  it('defaults the open duration when the breaker is enabled without one', () => {
    const c = clock();
    const b = CircuitBreaker.create({ failureThreshold: 1 }, c.now);
    if (!b) throw new Error('expected a breaker');
    b.record(false, false); // one failure crosses threshold 1 → open

    expect(b.allow().allowed).toBe(false); // open immediately
    c.advance(DEFAULT_BREAKER_OPEN_DURATION_MS - 1);
    expect(b.allow().allowed).toBe(false); // still open just before the default window
    c.advance(1);
    const decision = b.allow();
    expect(decision.allowed).toBe(true); // half-open trial admitted at the default window
    expect(decision.trial).toBe(true);
  });

  it('opens only after the failure threshold is crossed', () => {
    const b = CircuitBreaker.create({ failureThreshold: 3, openDurationMs: 3_600_000 });
    if (!b) throw new Error('expected a breaker');

    for (let i = 0; i < 2; i++) {
      expect(b.allow().allowed).toBe(true); // closed before the threshold
      b.record(false, false);
    }
    expect(b.allow().allowed).toBe(true); // still closed at 2 failures
    b.record(false, false); // third failure crosses the threshold
    expect(b.allow().allowed).toBe(false); // now open
  });

  it('resets the failure run on an intervening success', () => {
    const b = CircuitBreaker.create({ failureThreshold: 3, openDurationMs: 3_600_000 });
    if (!b) throw new Error('expected a breaker');

    b.record(false, false);
    b.record(false, false);
    b.record(true, false); // reset
    b.record(false, false);
    b.record(false, false);
    expect(b.allow().allowed).toBe(true); // only two failures since the reset
  });

  it('closes on a successful half-open trial and serializes the probe', () => {
    const c = clock();
    const b = CircuitBreaker.create({ failureThreshold: 2, openDurationMs: 3_600_000 }, c.now);
    if (!b) throw new Error('expected a breaker');

    b.record(false, false);
    b.record(false, false); // open
    c.advance(2 * 3_600_000); // window elapsed

    const trial = b.allow();
    expect(trial.allowed).toBe(true);
    expect(trial.trial).toBe(true);
    // A concurrent caller during the probe is denied — only one trial in flight.
    expect(b.allow().allowed).toBe(false);

    b.record(true, trial.trial); // probe succeeded → closed
    expect(b.allow().allowed).toBe(true);
  });

  it('re-opens on a failed half-open trial and frees the probe slot', () => {
    const c = clock();
    const b = CircuitBreaker.create({ failureThreshold: 2, openDurationMs: 3_600_000 }, c.now);
    if (!b) throw new Error('expected a breaker');

    b.record(false, false);
    b.record(false, false); // open
    c.advance(2 * 3_600_000);

    const trial = b.allow();
    b.record(false, trial.trial); // probe failed → re-open, free the slot
    expect(b.allow().allowed).toBe(false);

    // A later window admits a fresh trial (the slot was released).
    c.advance(2 * 3_600_000);
    const trial2 = b.allow();
    expect(trial2.allowed).toBe(true);
    expect(trial2.trial).toBe(true);
  });

  // A 429 is neither a success nor a failure: release() is the neutral
  // completion (Go: TestCircuitBreaker_ReleaseIsNeutralWhileClosed).
  it('treats release as neutral while closed: it neither counts nor resets the failure run', () => {
    const b = CircuitBreaker.create({ failureThreshold: 2, openDurationMs: 3_600_000 });
    if (!b) throw new Error('expected a breaker');

    b.record(false, false); // one genuine failure
    for (let i = 0; i < 10; i++) {
      b.release(false); // a run of throttled calls, far past the threshold
    }
    expect(b.allow()).toEqual({ allowed: true, trial: false }); // still closed: not counted

    b.record(false, false); // second genuine failure: the run survived, so it opens
    expect(b.allow().allowed).toBe(false);
  });

  // The wedge guard (Go: TestCircuitBreaker_HalfOpenTrialReleaseFreesProbeSlot).
  it('frees a throttled half-open probe slot without closing or re-arming the breaker', () => {
    const c = clock();
    const b = CircuitBreaker.create({ failureThreshold: 2, openDurationMs: 3_600_000 }, c.now);
    if (!b) throw new Error('expected a breaker');

    b.record(false, false);
    b.record(false, false); // open
    c.advance(2 * 3_600_000); // window elapsed

    const trial = b.allow();
    expect(trial).toEqual({ allowed: true, trial: true });
    b.release(false); // a caller that holds no probe slot must not free this one
    expect(b.allow().allowed).toBe(false);

    b.release(trial.trial); // the probe was throttled
    // Same instant, no clock advance: a re-armed window would deny, and a closed
    // breaker would admit without a trial. Only a freed slot admits a new trial.
    expect(b.allow()).toEqual({ allowed: true, trial: true });
  });
});
