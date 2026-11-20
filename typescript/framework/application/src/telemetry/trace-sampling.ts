/**
 * Trace sampling — the trace budget shared with the Go runtime.
 *
 * A root span is kept while the instance has kept fewer than `budget` roots,
 * or while the current UTC minute holds fewer than `floorPerMinute` kept
 * roots, or when `ratio` picks its trace ID. Otherwise it is dropped. A span
 * with a parent follows the parent's sampled flag and makes no decision,
 * except under the parent policy `decide` a server span whose remote parent
 * came from outside, which takes a root decision on the parent's trace.
 *
 * The rule, the keys, the defaults and the counter are the
 * telemetry/otlp-push-contract convention, committed in
 * protocols/telemetry/fixtures/conventions/trace-sampling.json.
 */

import { Config, Int, Optional } from '@putnami/runtime';
import { applyTraceSampling, type ExportDirective, getDirectiveReceiver } from './export-directive';
import { incCounterWithAttributes } from './telemetry.utils';

/** The config section that holds the three keys. */
export const TRACE_SAMPLING_CONFIG_PATH = 'telemetry.traces';
/** Counts each root decision, labeled {@link ATTR_TRACE_DECISION_OUTCOME}. */
export const TRACE_DECISIONS = 'trace.decisions';
/** The attribute that labels a decision kept or dropped. */
export const ATTR_TRACE_DECISION_OUTCOME = 'outcome';

/**
 * The parent policy. `follow` makes a span with a parent follow the parent's
 * sampled flag. `decide` makes a server span whose remote parent came from
 * outside — a `traceparent` with no `x-putnami-traceparent` on the same trace,
 * as a load balancer's edge or a browser sends — take a root decision, keeping
 * the parent's trace ID. A parent a Putnami client sent is still followed.
 */
export type TraceParentPolicy = 'follow' | 'decide';

/** The parent policies, in the order the protocol lists them. */
export const TRACE_PARENT_POLICIES: readonly TraceParentPolicy[] = Object.freeze(['follow', 'decide']);

/** The settings of the trace budget. */
export interface TraceSampling {
  /** The number of root traces the instance keeps first; a whole number of 0 or more. */
  budget: number;
  /** The share of root traces kept once the budget is spent, between 0 and 1. */
  ratio: number;
  /** The number of root traces kept each UTC minute whatever the ratio picks; a whole number of 0 or more. */
  floorPerMinute: number;
  /** The parent policy; absent reads as `follow`. */
  parent?: TraceParentPolicy;
}

/** The settings that apply when nothing sets them. */
export const DEFAULT_TRACE_SAMPLING: Readonly<TraceSampling> = Object.freeze({
  budget: 100,
  ratio: 0.01,
  floorPerMinute: 1,
  parent: 'follow',
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
  parent: Optional(String),
});

/** A `telemetry.traces` section as read: each key may be unset, and `parent` is checked later. */
export type TraceSamplingSection = Partial<Omit<TraceSampling, 'parent'>> & { parent?: string };

/** Returns base with each key the section sets. */
export function traceSamplingOver(section: TraceSamplingSection, base: Readonly<TraceSampling>): TraceSampling {
  const parent = section.parent ?? base.parent;
  return {
    budget: section.budget ?? base.budget,
    ratio: section.ratio ?? base.ratio,
    floorPerMinute: section.floorPerMinute ?? base.floorPerMinute,
    ...(parent === undefined ? {} : { parent: parent as TraceParentPolicy }),
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
  if (settings.parent !== undefined && !TRACE_PARENT_POLICIES.includes(settings.parent)) {
    return `${TRACE_SAMPLING_CONFIG_PATH}.parent must be follow or decide, got ${String(settings.parent)}`;
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

  /** The settings `update()` or the constructor set, without a directive's. */
  get settings(): Readonly<TraceSampling> {
    return this.current;
  }

  /**
   * Applies `directive`, which arrived at `nowMs`, to the decisions until its
   * ttl ends: each trace setting it carries replaces the matching setting,
   * higher or lower. It replaces a directive in force; the settings
   * themselves stay.
   */
  direct(directive: ExportDirective, nowMs: number = Date.now()): void {
    this.directive = directive;
    this.directiveEndsMs = nowMs + directive.ttlSeconds * 1000;
  }

  /** The settings a decision at `nowMs` applies: the settings, with those of a directive that still holds in their place. */
  inForce(nowMs: number = Date.now()): Readonly<TraceSampling> {
    if (this.directive && nowMs < this.directiveEndsMs) return applyTraceSampling(this.directive, this.current);
    return this.current;
  }

  /** Whether the parent policy in force at `nowMs` is `decide`. */
  decidesOutsideParents(nowMs: number = Date.now()): boolean {
    return this.inForce(nowMs).parent === 'decide';
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
