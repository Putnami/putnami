import { robustRemove, robustRename } from '@putnami/runtime/robustio';
import { joinPath } from '@putnami/utils';

// ---------------------------------------------------------------------------
// Infra requirements scratch fragment — events publish/subscribe contributions
// ---------------------------------------------------------------------------
//
// The infra-requirements protocol (protocols/infra) lets framework
// plugins declare, per project, the infrastructure their code needs. The
// events plugin contributes the set of topics a project publishes to and
// subscribes from. The TypeScript generator syncs the scratch fragment into
// committed infra/requirements.json before the build aggregator runs.

/** Current infra-requirements protocol version. */
export const INFRA_PROTOCOL_VERSION = 2;

/** JSON Schema reference for the per-project manifest. */
export const INFRA_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-infra.json';

/**
 * Canonical resource-name pattern shared by the infra protocol:
 * lowercase letters, digits, '-', '_', '.', '/'; 1–64 chars.
 */
const RESOURCE_NAME_PATTERN = /^[a-z0-9][a-z0-9_./-]{0,63}$/;

/**
 * Per-producer sidecar slug. Each framework producer owns one file at
 * `<project>/.gen/infra/<slug>.json`. See protocols/infra/README.md.
 */
const SIDECAR_SLUG = 'events';
const SIDECAR_RELATIVE_PATH = `.gen/infra/${SIDECAR_SLUG}.json`;

/** Raised when a topic name violates the canonical resource-name pattern. */
export class InvalidTopicNameError extends Error {
  constructor(public readonly names: string[]) {
    super(
      `Topic name(s) do not match the infra resource pattern ${RESOURCE_NAME_PATTERN.source}: ${names
        .map((n) => `'${n}'`)
        .join(', ')}`,
    );
    this.name = 'InvalidTopicNameError';
  }
}

/** Delivery model for a subscribed topic. Owned by protocols/events. */
export type Delivery = 'pull' | 'stream' | 'push';

/** A subscribed topic carrying a non-default delivery model. */
export interface InfraSubscription {
  topic: string;
  delivery: Delivery;
}

export interface EventsRequirements {
  publishes: string[];
  /**
   * Subscribed topics. A bare string is the canonical compact form (delivery
   * `pull`); an object carries an explicit non-default delivery so the deployer
   * can provision a provider push subscription.
   */
  subscribes: (string | InfraSubscription)[];
}

export interface InfraManifest {
  $schema: string;
  protocolVersion: number;
  events: EventsRequirements;
}

function normalize(names: Iterable<string>): string[] {
  return [...new Set(names)].sort();
}

/**
 * Build the per-project infra manifest for the events contribution.
 *
 * Topic names are deduplicated and sorted so the output is byte-deterministic
 * for a given input (the protocol requires idempotent sidecar content).
 * Returns `undefined` when the project neither publishes nor subscribes to any
 * topic — there is nothing to declare.
 *
 * @throws {InvalidTopicNameError} if any topic name violates the canonical
 *   resource-name pattern.
 */
export function buildEventsInfraManifest(
  publishes: Iterable<string>,
  subscribes: Iterable<string>,
  delivery: Delivery = 'pull',
): InfraManifest | undefined {
  return buildEventsInfraManifestFromSubscriptions(
    publishes,
    [...subscribes].map((topic) => ({ topic, delivery })),
  );
}

/** Raised when two plugins of one workload subscribe a topic with different deliveries. */
export class ConflictingDeliveryError extends Error {
  constructor(
    public readonly topic: string,
    public readonly deliveries: [Delivery, Delivery],
  ) {
    super(
      `Topic '${topic}' is subscribed with both '${deliveries[0]}' and '${deliveries[1]}' delivery; a workload provisions one delivery per topic`,
    );
    this.name = 'ConflictingDeliveryError';
  }
}

/**
 * Build the manifest from subscriptions that each carry their own delivery: the
 * union of every `events()` plugin of one workload. Same output as
 * {@link buildEventsInfraManifest} when every subscription shares one delivery.
 *
 * @throws {ConflictingDeliveryError} if one topic is subscribed with two
 *   different deliveries: it cannot be provisioned both ways.
 * @throws {InvalidTopicNameError} if any topic name violates the canonical
 *   resource-name pattern.
 */
export function buildEventsInfraManifestFromSubscriptions(
  publishes: Iterable<string>,
  subscriptions: Iterable<InfraSubscription>,
): InfraManifest | undefined {
  const deliveries = new Map<string, Delivery>();
  for (const { topic, delivery } of subscriptions) {
    const previous = deliveries.get(topic);
    if (previous !== undefined && previous !== delivery) {
      throw new ConflictingDeliveryError(topic, [previous, delivery]);
    }
    deliveries.set(topic, delivery);
  }
  const normalizedPublishes = normalize(publishes);
  const normalizedSubscribes = normalize(deliveries.keys());

  if (normalizedPublishes.length === 0 && normalizedSubscribes.length === 0) {
    return undefined;
  }

  const invalid = [...new Set([...normalizedPublishes, ...normalizedSubscribes])].filter(
    (name) => !RESOURCE_NAME_PATTERN.test(name),
  );
  if (invalid.length > 0) {
    throw new InvalidTopicNameError(invalid);
  }

  // A pull subscription emits a bare topic string so pull/stream workloads keep
  // a byte-for-byte identical manifest; only another delivery widens the form.
  const subscribesOut: (string | InfraSubscription)[] = normalizedSubscribes.map((topic) => {
    const delivery = deliveries.get(topic) ?? 'pull';
    return delivery === 'pull' ? topic : { topic, delivery };
  });

  return {
    $schema: INFRA_SCHEMA_URL,
    protocolVersion: INFRA_PROTOCOL_VERSION,
    events: {
      publishes: normalizedPublishes,
      subscribes: subscribesOut,
    },
  };
}

/** Resolve the sidecar path for a project root. */
export function infraSidecarPath(projectRoot: string): string {
  return joinPath(projectRoot, SIDECAR_RELATIVE_PATH);
}

/**
 * Write the manifest to `<projectRoot>/.gen/infra/events.json`.
 *
 * The protocol requires an atomic write: the same generate task may run
 * concurrently for the build and test pipelines while the generator sync reads
 * scratch fragments. Writing to a temp file and renaming guarantees readers
 * never observe a torn write. Returns the sidecar path.
 */
export async function writeInfraSidecar(projectRoot: string, manifest: InfraManifest): Promise<string> {
  const target = infraSidecarPath(projectRoot);
  const tmp = `${target}.${process.pid}.${Date.now()}.tmp`;
  const json = `${JSON.stringify(manifest, null, 2)}\n`;

  // Bun.write creates parent directories as needed.
  await Bun.write(tmp, json);
  await robustRename(tmp, target);

  return target;
}

/**
 * Remove the sidecar at `<projectRoot>/.gen/infra/events.json` if present.
 *
 * Called when a project no longer publishes or subscribes to any topic (e.g.
 * its last handler was deleted): a stale sidecar left on disk would otherwise
 * keep contributing removed topics to generator sync. A missing file is a
 * no-op. Returns the resolved sidecar path.
 */
export async function removeInfraSidecar(projectRoot: string): Promise<string> {
  const target = infraSidecarPath(projectRoot);
  await robustRemove(target);
  return target;
}
