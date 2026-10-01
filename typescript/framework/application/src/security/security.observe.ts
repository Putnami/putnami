/**
 * Per-request authorization observability — the TypeScript twin of
 * `go/framework/security` observe.go.
 *
 * Authorization is a security-critical operation, so every allow/deny decision
 * is both logged (structured, via the framework logger) and counted (through the
 * in-process telemetry collector, so the totals are aggregated per-second and
 * flushed by the existing telemetry sender — never a per-request OTLP post).
 * Grants log at debug to keep the steady-state log quiet; denials log at warn so
 * an operator can spot 401/403 spikes (credential stuffing, brute force) and
 * attribute them to the failing rule.
 *
 * Redaction contract (ported from observe.go's doc comment): the log carries the
 * subject ("anonymous" when unauthenticated), the HTTP decision (401/403) and
 * the failing dimension (the reason), but NEVER the bearer token, secret, or the
 * exact scope/role that was missing. That detail stays server-side (logs and
 * telemetry) and is never returned to the client, which only ever sees a bare
 * 401/403 body.
 */

import { useLogger } from '@putnami/runtime';
import type { HttpRequestContext } from '../http/http-context.type';
import { incCounter } from '../telemetry/telemetry.utils';
import { AuthDecision } from './identity.constants';

/** The metric namespace under which auth outcomes accumulate. */
const AUTH_DECISION_COUNTER = 'security.auth_decisions';

/**
 * The HTTP status a decision maps to: 401 for the unauthenticated case, 200 for
 * a grant, 403 for any rule failure. Mirrors observe.go's `decision.status()`.
 */
function decisionStatus(d: AuthDecision): number {
  switch (d) {
    case AuthDecision.Allow:
      return 200;
    case AuthDecision.DenyUnauthenticated:
      return 401;
    default:
      return 403;
  }
}

/** Fold a free-text reason into a stable, low-cardinality metric-key segment. */
function reasonSlug(reason: string): string {
  return reason.trim().replace(/\s+/g, '_');
}

/**
 * The metric key for one auth outcome: the decision label plus the failing
 * dimension (the reason). Grants carry no reason and key on the label alone, so
 * an operator can read per-decision totals (`security.auth_decisions.allow`,
 * `security.auth_decisions.deny_role.*`) without the token or the exact missing
 * scope/role ever entering the key.
 */
function decisionCounterKey(d: AuthDecision, reason: string): string {
  return reason ? `${AUTH_DECISION_COUNTER}.${d}.${reasonSlug(reason)}` : `${AUTH_DECISION_COUNTER}.${d}`;
}

/**
 * Count and log a single authorization outcome. `reason` names the failing
 * dimension (client/scope/role/guard/...) for denials and is empty for grants;
 * it must not contain a token, secret, or the precise missing scope/role value.
 */
export function recordDecision(ctx: HttpRequestContext, d: AuthDecision, reason: string): void {
  // In-process aggregate (no-op when telemetry is not initialised); the
  // telemetry sender flushes it, so this is never a per-request network call.
  incCounter(decisionCounterKey(d, reason));

  const rawSubject = ctx.user?.sub;
  const subject = typeof rawSubject === 'string' && rawSubject !== '' ? rawSubject : 'anonymous';

  const attrs: Record<string, unknown> = {
    decision: d,
    status: decisionStatus(d),
    subject,
    method: ctx.method,
    path: ctx.path(),
  };
  if (reason) {
    attrs['reason'] = reason;
  }

  const log = useLogger('security');
  if (d === AuthDecision.Allow) {
    log.debug('authorization granted', attrs);
    return;
  }
  log.warn('authorization denied', attrs);
}
