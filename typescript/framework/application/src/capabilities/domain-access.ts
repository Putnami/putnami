import type { DomainAccessEnforcementV2, DomainAccessTransportV2 } from './manifest.types';

/**
 * One Domain Access & Replication Contract a plugin enforces at run time.
 *
 * It is EVIDENCE, not authority. The contract itself is declared and reviewed in
 * a `putnami.architecture.json`; a declaration here only states that a running
 * component was configured with it, so `putnami architecture validate` can tell a
 * declared import nothing implements from an implemented one nobody declared.
 * Emitting a row never creates a permission — that is the anti-pattern ADR 0001
 * of `protocols/architecture` exists to forbid.
 *
 * Every member is carried VERBATIM from the declared contract. This module does
 * not restate the ARC/DARC vocabulary and never validates it: what a mode or a
 * failure behavior means belongs to the architecture protocol, and a second copy
 * is exactly the drift a checker joining the two documents exists to catch.
 * `../darc` builds these values from a contract it already validated.
 *
 * The Go twin is `app.DomainAccessContract`.
 */
export interface DomainAccessDeclaration {
  /** The architecture import ID the component enforces. */
  readonly import: string;
  /** The declared access mode. */
  readonly mode: string;
  /** The declared lifecycle status of the import. */
  readonly status: string;
  /** The carriers the contract declares, each with its role. */
  readonly transports?: readonly DomainAccessTransportV2[];
  /** The contract parameters the component actually applies. */
  readonly enforced?: DomainAccessEnforcementV2;
}

/**
 * Implemented by plugins that enforce a domain access contract at run time. The
 * capabilities producer walks every contributor in the module tree and records
 * its declarations as manifest `domainAccess` rows, stamping the identity and
 * provenance every other contribution carries.
 */
export interface DomainAccessContributor {
  domainAccessContracts(): readonly DomainAccessDeclaration[];
}

/** Runtime type guard for {@link DomainAccessContributor}. */
export function isDomainAccessContributor(value: unknown): value is DomainAccessContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'domainAccessContracts' in value &&
    typeof (value as DomainAccessContributor).domainAccessContracts === 'function'
  );
}
