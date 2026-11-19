/**
 * Trace sampling — the trace budget shared with the Go runtime.
 *
 * A root span is kept while the instance has kept fewer than `budget` roots,
 * or while the current UTC minute holds fewer than `floorPerMinute` kept
 * roots, or when `ratio` picks its trace ID. Otherwise it is dropped. A span
 * with a parent follows the parent's sampled flag and makes no decision.
 *
 * The rule, the keys, the defaults and the counter are the
 * telemetry/otlp-push-contract convention, committed in
 * protocols/telemetry/fixtures/conventions/trace-sampling.json.
 */

import { Config, Int, Optional } from '@putnami/runtime';
import { type ExportDirective, getDirectiveReceiver, limitTraceSampling } from './export-directive';
import { incCounterWithAttributes } from './telemetry.utils';

/** The config section that holds the three keys. */
export const TRACE_SAMPLING_CONFIG_PATH = 'telemetry.traces';
/** Counts each root decision, labeled {@link ATTR_TRACE_DECISION_OUTCOME}. */
export const TRACE_DECISIONS = 'trace.decisions';
/** The attribute that labels a decision kept or dropped. */
export const ATTR_TRACE_DECISION_OUTCOME = 'outcome';

/** The three settings of the trace budget. */
export interface TraceSampling {
  /** The number of root traces the instance keeps first; a whole number of 0 or more. */
  budget: number;
  /** The share of root traces kept once the budget is spent, between 0 and 1. */
  ratio: number;
  /** The number of root traces kept each UTC minute whatever the ratio picks; a whole number of 0 or more. */
  floorPerMinute: number;
}

/** The settings that apply when nothing sets them. */
export const DEFAULT_TRACE_SAMPLING: Readonly<TraceSampling> = Object.freeze({
  budget: 100,
  ratio: 0.01,
  floorPerMinute: 1,
});

/**
 * The `telemetry.traces` config section. Each key it sets overrides the
 * matching field of the `traces` option of `telemetry()`; a key it leaves out
 * keeps that field.
 */
export const TraceSamplingConfig = Config(TRACE_SAMPLING_CONFIG_PATH, {
  budget: Optional(Int),
  ratio: Optional(Number),
  floorPerMinute: Optional(Int),
});

/** A `telemetry.traces` section as read: each key may be unset. */
export type TraceSamplingSection = Partial<TraceSampling>;

/** Returns base with each key the section sets. */
export function traceSamplingOver(section: TraceSamplingSection, base: Readonly<TraceSampling>): TraceSampling {
  return {
    budget: section.budget ?? base.budget,
    ratio: section.ratio ?? base.ratio,
    floorPerMinute: section.floorPerMinute ?? base.floorPerMinute,
  };
}

/** Reports a setting out of range, or `undefined` when every setting is in range. */
export function traceSamplingError(settings: Readonly<TraceSampling>): string | undefined {
  if (!Number.isInteger(settings.budget) || settings.budget < 0) {
    return `${TRACE_SAMPLING_CONFIG_PATH}.budget must be a whole number of 0 or more, got ${settings.budget}`;
  }
  if (typeof settings.ratio !== 'number' || Number.isNaN(settings.ratio) || settings.ratio < 0 || settings.ratio > 1) {
    return `${TRACE_SAMPLING_CONFIG_PATH}.ratio must lie between 0 and 1, got ${settings.ratio}`;
  }
  if (!Number.isInteger(settings.floorPerMinute) || settings.floorPerMinute < 0) {
    return `${TRACE_SAMPLING_CONFIG_PATH}.floorPerMinute must be a whole number of 0 or more, got ${settings.floorPerMinute}`;
  }
  return undefined;
}

const TWO_POW_63 = 2 ** 63;
const TRACE_ID = /^[0-9a-f]{32}$/i;

/**
 * Reports whether `ratio` picks a trace ID of 32 hex digits: its last 8 bytes,
 * read as a big-endian unsigned integer and shifted right one bit, are below
 * `ratio` times 2^63. It is the test of the OpenTelemetry TraceIDRatioBased
 * sampler, so every runtime picks the same trace IDs. A string that is not 32
 * hex digits is never picked.
 */
export function traceRatioPicks(traceId: string, ratio: number): boolean {
  if (!TRACE_ID.test(traceId)) return false;
  if (ratio >= 1) return true;
  if (!(ratio > 0)) return false;
  const bound = BigInt(Math.trunc(ratio * TWO_POW_63));
  return BigInt(`0x${traceId.slice(16, 32)}`) >> 1n < bound;
}

/**
 * Applies the sampling rule to the root spans of one instance and counts each
 * decision in `trace.decisions` on the active collector.
 *
 * `settings` returns the settings in force, and `update()` replaces them for
 * the next root decision. The telemetry plugin calls `update()` when the
 * `telemetry.traces` config section changes; a caller that tightens the budget
 * at runtime calls it too, and its settings hold until that section changes
 * again.
 */
export class TraceSampler {
  private current: Readonly<TraceSampling>;
  private kept = 0;
  private minute = Number.NEGATIVE_INFINITY;
  private keptInMinute = 0;
  private directive: ExportDirective | undefined;
  private directiveEndsMs = Number.NEGATIVE_INFINITY;

  /** @throws when a setting is out of range. */
  constructor(settings: Readonly<TraceSampling> = DEFAULT_TRACE_SAMPLING) {
    const error = traceSamplingError(settings);
    if (error) throw new Error(error);
    this.current = Object.freeze({ ...settings });
  }

  /** The settings `update()` or the constructor set, without a directive's limits. */
  get settings(): Readonly<TraceSampling> {
    return this.current;
  }

  /**
   * Applies `directive`, which arrived at `nowMs`, to the decisions until its
   * ttl ends: each one takes the stricter of the directive and the settings.
   * It replaces a directive in force; the settings themselves stay.
   */
  restrict(directive: ExportDirective, nowMs: number = Date.now()): void {
    this.directive = directive;
    this.directiveEndsMs = nowMs + directive.ttlSeconds * 1000;
  }

  /** The settings a decision at `nowMs` applies: the settings, limited by a directive that still holds. */
  inForce(nowMs: number = Date.now()): Readonly<TraceSampling> {
    if (this.directive && nowMs < this.directiveEndsMs) return limitTraceSampling(this.directive, this.current);
    return this.current;
  }

  /**
   * Applies settings to the next root decision. The roots already kept still
   * count against the new budget and floor.
   *
   * @throws when a setting is out of range; the settings in force stay.
   */
  update(settings: Readonly<TraceSampling>): void {
    const error = traceSamplingError(settings);
    if (error) throw new Error(error);
    this.current = Object.freeze({ ...settings });
  }

  /** Reports whether the root span of `traceId`, started at `nowMs`, is kept, and counts the decision. */
  decide(traceId: string, nowMs: number = Date.now()): boolean {
    const minute = Math.floor(nowMs / 60_000);
    if (minute !== this.minute) {
      this.minute = minute;
      this.keptInMinute = 0;
    }
    const settings = this.inForce(nowMs);
    const keep =
      this.kept < settings.budget ||
      this.keptInMinute < settings.floorPerMinute ||
      traceRatioPicks(traceId, settings.ratio);
    if (keep) {
      this.kept++;
      this.keptInMinute++;
    }
    incCounterWithAttributes(TRACE_DECISIONS, 1, { [ATTR_TRACE_DECISION_OUTCOME]: keep ? 'kept' : 'dropped' });
    return keep;
  }
}

// The span API is process-global, like the collector it records on, so the
// sampler the telemetry plugin installs is process-global too.
let _sampler: TraceSampler | undefined;

/** The sampler that decides root spans, or `undefined` when no telemetry plugin runs. */
export function getTraceSampler(): TraceSampler | undefined {
  return _sampler;
}

/** @internal Used by TelemetryPlugin to install its sampler. */
export function setTraceSampler(sampler: TraceSampler): void {
  _sampler = sampler;
  getDirectiveReceiver().follow(sampler);
}

/** @internal Removes the sampler only if it is still the one installed. */
export function clearTraceSampler(sampler: TraceSampler): void {
  if (_sampler === sampler) {
    _sampler = undefined;
    getDirectiveReceiver().follow(undefined);
  }
}
