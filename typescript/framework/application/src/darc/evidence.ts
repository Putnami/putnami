/**
 * The evidence half: what a registered component tells the describe pass.
 *
 * A component enforces a contract at run time; the capability manifest records
 * THAT IT DOES, as one machine row. The row is evidence and never authority —
 * `putnami architecture validate` reads it beside the declared imports and
 * reports a declared active contract nothing implements, or an implemented one
 * nobody declared. Emitting a row cannot create a cross-domain permission; only a
 * reviewed manifest edit can (ADR 0001).
 *
 * The capability producer stays the sole committer of the sidecar:
 * nothing here writes a file, invents an artifact kind, or opens a second
 * channel. A component hands its already-validated contract to a plugin, the
 * plugin hands it to the producer, and the producer emits.
 *
 * The Go twin is `go.putnami.dev/app/darc/evidence.go`.
 */

import type { Plugin } from '../application/module.types';
import type { DomainAccessContributor, DomainAccessDeclaration } from '../capabilities/domain-access';
import type { DomainAccessTransportV2 } from '../capabilities/manifest.types';
import type { Command } from './command';
import type { Projection } from './projection';
import type { Reference } from './reference';
import type { Snapshot } from './snapshot';

/**
 * One runtime-enforced Domain Access & Replication Contract.
 *
 * The union is closed to this module's four components on purpose: a value that
 * is not one of them cannot claim to implement a contract.
 */
export type DarcComponent =
  | Reference
  // biome-ignore lint/suspicious/noExplicitAny: the payload type is irrelevant to the contract a component enforces
  | Projection<any>
  // biome-ignore lint/suspicious/noExplicitAny: the payload type is irrelevant to the contract a component enforces
  | Snapshot<any>
  // biome-ignore lint/suspicious/noExplicitAny: the payload type is irrelevant to the contract a component enforces
  | Command<any>;

/**
 * Project one enforced contract onto the row the capability manifest carries.
 *
 * Every value is carried verbatim from the contract the component was
 * constructed with — and that contract already passed the protocol's own
 * validation, so the row cannot claim a mode or a behavior the gate would reject.
 */
export function evidence(component: DarcComponent): DomainAccessDeclaration {
  const contract = component.contract();
  const transports: DomainAccessTransportV2[] = [];
  for (const [role, carrier] of [
    ['bootstrap', contract.bootstrap],
    ['transport', contract.transport],
    ['updates', contract.updates],
  ] as const) {
    if (!carrier) continue;
    transports.push({
      role,
      kind: carrier.kind,
      ...(carrier.contract === undefined ? {} : { contract: carrier.contract }),
      availability: carrier.availability,
    });
  }
  transports.sort((left, right) => (left.role === right.role ? 0 : left.role < right.role ? -1 : 1));

  const enforced: Record<string, string> = {};
  if (contract.consistency) {
    enforced['maxStaleness'] = contract.consistency.maxStaleness;
    enforced['onMissing'] = contract.consistency.onMissing;
    enforced['onStale'] = contract.consistency.onStale;
    enforced['ordering'] = contract.consistency.ordering;
    enforced['lateEvents'] = contract.consistency.lateEvents;
  }
  if (contract.deletion) enforced['deletion'] = contract.deletion.strategy;
  if (contract.localModel) {
    enforced['writer'] = contract.localModel.writer;
    enforced['rebuild'] = contract.localModel.rebuild;
  }

  return {
    import: contract.id,
    mode: contract.mode,
    status: contract.status,
    ...(transports.length > 0 ? { transports } : {}),
    // An all-empty enforcement block is omitted rather than emitted as an object
    // of empty strings: a reference contract enforces none of these parameters,
    // and saying so with an absent member is honest.
    ...(Object.keys(enforced).length > 0 ? { enforced } : {}),
  };
}

/**
 * Carries a project's enforced contracts into the capability manifest.
 *
 * It declares no lifecycle hook. Registering it with `use` is how a
 * workload says "these contracts are implemented here", and the producer is what
 * turns that into a committed row.
 *
 * ```typescript
 * app.use(new DarcPlugin('observability-contracts', projection, usageCommand));
 * ```
 *
 * A component registered twice is registered once: the manifest identity is the
 * import and its mode, and a duplicate would be the same statement twice.
 */
export class DarcPlugin implements Plugin, DomainAccessContributor {
  readonly name: string;
  private readonly components: DarcComponent[] = [];

  constructor(name: string, ...components: DarcComponent[]) {
    this.name = name;
    this.register(...components);
  }

  /** Add components. A component whose (import, mode) pair is already registered is ignored. */
  register(...components: DarcComponent[]): this {
    const seen = new Set(this.components.map(componentKey));
    for (const component of components) {
      if (!component || seen.has(componentKey(component))) continue;
      seen.add(componentKey(component));
      this.components.push(component);
    }
    return this;
  }

  /** Every registered contract, ordered by import and mode so the rows are deterministic. */
  domainAccessContracts(): DomainAccessDeclaration[] {
    return this.components
      .map(evidence)
      .sort((left, right) =>
        left.import === right.import
          ? compareStrings(left.mode, right.mode)
          : compareStrings(left.import, right.import),
      );
  }
}

function compareStrings(left: string, right: string): number {
  if (left === right) return 0;
  return left < right ? -1 : 1;
}

function componentKey(component: DarcComponent): string {
  const contract = component.contract();
  return `${contract.id} ${contract.mode}`;
}
