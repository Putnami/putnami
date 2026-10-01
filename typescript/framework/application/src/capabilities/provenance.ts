import type { Provenance } from './manifest.types';

/** Optional best-effort provenance a TypeScript contribution can declare. */
export type CapabilityProvenanceMetadata = Omit<Partial<Provenance>, 'project'>;

/**
 * Implemented by plugins that can identify the package/evidence owning their
 * capability contributions. The manifest's project remains framework-owned;
 * omitted metadata preserves the existing project-only provenance behavior.
 */
export interface CapabilityProvenanceContributor {
  capabilityProvenance(): CapabilityProvenanceMetadata;
}

export function isCapabilityProvenanceContributor(value: unknown): value is CapabilityProvenanceContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'capabilityProvenance' in value &&
    typeof (value as CapabilityProvenanceContributor).capabilityProvenance === 'function'
  );
}
