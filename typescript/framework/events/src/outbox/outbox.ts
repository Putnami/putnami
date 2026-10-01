import { getExternalCaller, getProjectRoot } from '@putnami/utils';
import type { TopicDefinition } from '../topic/topic';

// ---------------------------------------------------------------------------
// OutboxDefinition — the declaration of a transactional outbox
// ---------------------------------------------------------------------------

const OUTBOX_MARKER = 'putnami:outbox' as const;

/**
 * A transactional outbox declaration. Like {@link TopicDefinition} it is pure
 * data: it owns no table, transaction, relay loop, or retry policy. Writing the
 * row and relaying it stay in the application code that already does so.
 *
 * The declaration exists so the design graph can tell two different facts
 * apart. A row enqueued inside a business transaction is durable at commit
 * time; the topic is published only later, when a relay claims that row. The
 * two are separate relationships, not one `publishes` edge.
 */
export interface OutboxDefinition {
  readonly __outbox: typeof OUTBOX_MARKER;
  readonly name: string;
  /** Topics this outbox's relay publishes once a staged row is claimed. */
  readonly topics: readonly TopicDefinition[];
  /** Optional relation the rows are staged in, e.g. `auth.event_outbox`. */
  readonly table?: string;
  /** Optional datasource identifier owning {@link table}. */
  readonly datasource?: string;
  /** @internal Native declaration location for build-time design discovery. */
  readonly __source?: { path: string; line?: number; symbol?: string };
}

export function isOutboxDefinition(value: unknown): value is OutboxDefinition {
  return typeof value === 'object' && value !== null && (value as OutboxDefinition).__outbox === OUTBOX_MARKER;
}

export interface OutboxOptions {
  /** Topics the relay publishes for rows staged in this outbox. */
  topics: readonly TopicDefinition[];
  /** Relation the rows are staged in, e.g. `auth.event_outbox`. */
  table?: string;
  /** Datasource identifier owning the staging relation. */
  datasource?: string;
}

// ---------------------------------------------------------------------------
// outbox() — declare a transactional outbox
// ---------------------------------------------------------------------------

/**
 * Declare a transactional outbox.
 *
 * The declaration is runtime-inert. Registering it on the events plugin adds no
 * transport, transaction, relay, retry, or handler behavior; it only lets the
 * build-time design graph model commit-time enqueue separately from relay-time
 * publication.
 *
 * @param name - Stable outbox name, unique within the project
 * @param options - Declared topics plus optional table/datasource identifiers
 * @returns A pure-data OutboxDefinition
 *
 * @example
 * ```typescript
 * import { outbox, topic, Uuid } from '@putnami/events';
 *
 * const SessionRevoked = topic('identity.session.revoked', { sessionId: Uuid });
 * export const IdentityOutbox = outbox('identity', {
 *   topics: [SessionRevoked],
 *   table: 'auth.event_outbox',
 *   datasource: 'identity',
 * });
 * ```
 */
export function outbox(name: string, options: OutboxOptions): OutboxDefinition {
  const caller = getExternalCaller(getProjectRoot());
  return {
    __outbox: OUTBOX_MARKER,
    name,
    topics: [...options.topics],
    table: options.table,
    datasource: options.datasource,
    ...(caller
      ? {
          __source: {
            path: caller.filePath,
            line: caller.lineNumber,
            ...(caller.functionName ? { symbol: caller.functionName } : {}),
          },
        }
      : {}),
  };
}
