/**
 * Export directives — the settings an OTLP endpoint returns in export responses.
 *
 * An export response may carry a `Putnami-Export-Directive` header that sets
 * the trace settings and tightens the log level of the service that sent it,
 * for a bounded time, and a 429 with a `Retry-After` slows one signal down.
 * Putnami Cloud renders the settings a person stored, so a directive's trace
 * settings win over the service's own, higher or lower. Every push, the log
 * sink included, hands its response to the process receiver: a valid
 * directive replaces the one in force, and until its ttl ends each trace
 * setting it carries replaces that of the installed trace sampler, and every
 * logger that checks the level takes the stricter of the directive's level
 * and its own. A void header changes nothing.
 *
 * The header, the rule and the Retry-After delay are the
 * telemetry/otlp-push-contract convention, shared with the Go runtime and
 * committed in protocols/telemetry/fixtures/conventions/export-directives.json.
 */

import { LEVEL_PRIORITY, type LogLevel, setLogLevelLimit } from '@putnami/runtime';
import type { TraceParentPolicy, TraceSampling } from './trace-sampling';

/** The response header that carries a directive. */
export const EXPORT_DIRECTIVE_HEADER = 'Putnami-Export-Directive';
/** Sets the trace budget: a whole number of 0 or more. */
export const DIRECTIVE_TRACE_BUDGET = 'trace-budget';
/** Sets the trace ratio: a number in [0, 1]. */
export const DIRECTIVE_TRACE_RATIO = 'trace-ratio';
/** Sets the trace floor: a whole number of 0 or more. */
export const DIRECTIVE_TRACE_FLOOR = 'trace-floor';
/** Sets the parent policy: follow or decide. */
export const DIRECTIVE_TRACE_PARENT = 'trace-parent';
/** Raises the minimum log level: debug, info, warn or error. */
export const DIRECTIVE_LOG_LEVEL = 'log-level';
/** How many seconds the directive holds: a whole number of 1 or more; required. */
export const DIRECTIVE_TTL = 'ttl';
/** The longest a directive holds, in seconds. */
export const MAX_DIRECTIVE_TTL_SECONDS = 3600;
/** The longest delay a 429 Retry-After imposes on a signal, in seconds. */
export const MAX_RETRY_AFTER_SECONDS = 3600;

/** One parsed directive. An absent setting leaves the service's own. */
export interface ExportDirective {
  /** Sets the trace budget. */
  traceBudget?: number;
  /** Sets the trace ratio. */
  traceRatio?: number;
  /** Sets the trace floor per minute. */
  traceFloor?: number;
  /** Sets the parent policy. */
  traceParent?: TraceParentPolicy;
  /** Raises the minimum log level. */
  logLevel?: LogLevel;
  /** How long the directive holds from its arrival, in seconds. */
  ttlSeconds: number;
}

const LOG_LEVELS: readonly LogLevel[] = ['debug', 'info', 'warn', 'error'];
const DIGITS = /^[0-9]+$/;
const DECIMAL = /^[0-9]*(\.[0-9]*)?$/;
const IMF_FIXDATE =
  /^(Mon|Tue|Wed|Thu|Fri|Sat|Sun), [0-9]{2} (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) [0-9]{4} [0-9]{2}:[0-9]{2}:[0-9]{2} GMT$/;

/** A whole number of 0 or more in ASCII digits, or `undefined`. A huge one stays huge, so a cap applies. */
function wholeNumber(raw: string): number | undefined {
  return DIGITS.test(raw) ? Number(raw) : undefined;
}

/**
 * Parses the value of {@link EXPORT_DIRECTIVE_HEADER}. Returns `undefined`
 * when the header is void: a known key with a malformed value, a repeated key,
 * a member that is not key=value, or no valid ttl. An unknown key is ignored,
 * and a ttl above {@link MAX_DIRECTIVE_TTL_SECONDS} is capped.
 */
export function parseExportDirective(value: string): ExportDirective | undefined {
  const directive: Partial<ExportDirective> = {};
  const seen = new Set<string>();
  for (const part of value.split(',')) {
    const member = part.trim();
    if (member === '') continue;
    const equals = member.indexOf('=');
    if (equals < 0) return undefined;
    const key = member.slice(0, equals).trim().toLowerCase();
    const raw = member.slice(equals + 1).trim();
    if (seen.has(key)) return undefined;
    seen.add(key);
    switch (key) {
      case DIRECTIVE_TRACE_BUDGET: {
        const budget = wholeNumber(raw);
        if (budget === undefined) return undefined;
        directive.traceBudget = budget;
        break;
      }
      case DIRECTIVE_TRACE_PARENT: {
        const parent = raw.toLowerCase();
        if (parent !== 'follow' && parent !== 'decide') return undefined;
        directive.traceParent = parent;
        break;
      }
      case DIRECTIVE_TRACE_RATIO: {
        if (!DECIMAL.test(raw) || raw === '' || raw === '.') return undefined;
        const ratio = Number(raw);
        if (!(ratio >= 0 && ratio <= 1)) return undefined;
        directive.traceRatio = ratio;
        break;
      }
      case DIRECTIVE_TRACE_FLOOR: {
        const floor = wholeNumber(raw);
        if (floor === undefined) return undefined;
        directive.traceFloor = floor;
        break;
      }
      case DIRECTIVE_LOG_LEVEL: {
        const level = raw.toLowerCase();
        if (!(LOG_LEVELS as readonly string[]).includes(level)) return undefined;
        directive.logLevel = level as LogLevel;
        break;
      }
      case DIRECTIVE_TTL: {
        const seconds = wholeNumber(raw);
        if (seconds === undefined || seconds < 1) return undefined;
        directive.ttlSeconds = Math.min(seconds, MAX_DIRECTIVE_TTL_SECONDS);
        break;
      }
    }
  }
  return directive.ttlSeconds === undefined ? undefined : (directive as ExportDirective);
}

/**
 * `own` with each trace setting the directive carries in its place, higher or
 * lower: the budget, the ratio, the floor and the parent policy. A setting the
 * directive leaves out keeps `own`'s.
 */
export function applyTraceSampling(directive: ExportDirective, own: Readonly<TraceSampling>): TraceSampling {
  const parent = directive.traceParent ?? own.parent;
  return {
    budget: directive.traceBudget ?? own.budget,
    ratio: directive.traceRatio ?? own.ratio,
    floorPerMinute: directive.traceFloor ?? own.floorPerMinute,
    ...(parent === undefined ? {} : { parent }),
  };
}

/** The stricter of `own` and the directive's level: the higher one. Error is the highest a directive sets. */
export function limitLogLevel(directive: ExportDirective, own: LogLevel): LogLevel {
  if (directive.logLevel && LEVEL_PRIORITY[directive.logLevel] > LEVEL_PRIORITY[own]) return directive.logLevel;
  return own;
}

/**
 * How long a signal waits before its next export after a response with
 * `status` and the `Retry-After` value, in ms: delta-seconds or an IMF-fixdate
 * HTTP-date, for a 429 only, at most {@link MAX_RETRY_AFTER_SECONDS}. Any
 * other status, or a value of neither form, gives 0.
 */
export function retryAfterDelayMs(status: number, value: string | null | undefined, nowMs: number): number {
  if (status !== 429 || value === null || value === undefined) return 0;
  const text = value.trim();
  const maxMs = MAX_RETRY_AFTER_SECONDS * 1000;
  const seconds = wholeNumber(text);
  if (seconds !== undefined) return Math.min(seconds * 1000, maxMs);
  if (!IMF_FIXDATE.test(text)) return 0;
  // Only the exact IMF-fixdate a formatter writes back is a date, so an
  // impossible day or a wrong weekday reads as no delay, as in Go.
  const at = Date.parse(text);
  if (Number.isNaN(at) || new Date(at).toUTCString() !== text) return 0;
  return Math.min(Math.max(0, at - nowMs), maxMs);
}

/** What a sampler needs to follow the directives. */
export interface DirectiveFollower {
  direct(directive: ExportDirective, nowMs: number): void;
}

/**
 * Holds the directive in force and applies it to the follower and to the log
 * level limit.
 */
export class DirectiveReceiver {
  private directive: ExportDirective | undefined;
  private arrivedMs = 0;
  private endsMs = 0;
  private follower: DirectiveFollower | undefined;

  constructor(private readonly now: () => number = Date.now) {}

  /** Applies the directive header of a collector response, if any. */
  observe(headers: Pick<Headers, 'get'> | undefined): void {
    const value = headers?.get(EXPORT_DIRECTIVE_HEADER);
    if (value !== null && value !== undefined) this.receive(value);
  }

  /** Parses `value` and, when it is a valid directive, applies it. */
  receive(value: string): void {
    const directive = parseExportDirective(value);
    if (!directive) return;
    const now = this.now();
    this.directive = directive;
    this.arrivedMs = now;
    this.endsMs = now + directive.ttlSeconds * 1000;
    this.follower?.direct(directive, now);
    if (directive.logLevel) {
      // Loggers consult the limit on every record only once a directive has
      // set a level. Each such directive installs it again, so a caller that
      // removed the limit does not turn the directives off for good.
      setLogLevelLimit(this.logLevelLimit);
    }
  }

  /** The directive's log level while it holds, else `undefined`. */
  readonly logLevelLimit = (): LogLevel | undefined =>
    this.directive && this.now() < this.endsMs ? this.directive.logLevel : undefined;

  /** Makes `follower` follow the directives, starting with the one in force, if any. */
  follow(follower: DirectiveFollower | undefined): void {
    this.follower = follower;
    if (follower && this.directive && this.now() < this.endsMs) follower.direct(this.directive, this.arrivedMs);
  }
}

let processDirectives = new DirectiveReceiver();

/** The receiver every push of the process hands its responses to. */
export function getDirectiveReceiver(): DirectiveReceiver {
  return processDirectives;
}

/** @internal Replaces the process receiver (for tests); returns the previous one. */
export function setDirectiveReceiver(receiver: DirectiveReceiver): DirectiveReceiver {
  const previous = processDirectives;
  processDirectives = receiver;
  return previous;
}

/**
 * The Retry-After delays of the signals, keyed by collector endpoint and
 * signal path, so a 429 on one signal delays only that signal.
 */
export class SignalDelays {
  private readonly until = new Map<string, number>();

  constructor(private readonly now: () => number = Date.now) {}

  /** Whether an export of `path` to `endpoint` now falls in a delay. */
  delayed(endpoint: string, path: string): boolean {
    const until = this.until.get(`${endpoint}|${path}`);
    return until !== undefined && this.now() < until;
  }

  /** Starts the delay a response with `status` and `retryAfter` asks for. */
  observe(endpoint: string, path: string, status: number, retryAfter: string | null | undefined): void {
    const now = this.now();
    const delay = retryAfterDelayMs(status, retryAfter, now);
    if (delay > 0) this.until.set(`${endpoint}|${path}`, now + delay);
  }
}

let processDelays = new SignalDelays();

/** The Retry-After delays every push of the process honors. */
export function getSignalDelays(): SignalDelays {
  return processDelays;
}

/** @internal Replaces the process delays (for tests); returns the previous ones. */
export function setSignalDelays(delays: SignalDelays): SignalDelays {
  const previous = processDelays;
  processDelays = delays;
  return previous;
}
