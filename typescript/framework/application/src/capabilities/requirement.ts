import type { CapabilityKind } from './manifest.types';

/** One author-facing required-capability declaration (provenance is emitter-owned). */
export interface CapabilityRequirement {
  name: string;
  requires: CapabilityKind[];
}

/** Plugin contract for build-time capability completeness requirements. */
export interface RequiredCapabilityContributor {
  requiredCapabilities(): CapabilityRequirement[];
}

export function isRequiredCapabilityContributor(value: unknown): value is RequiredCapabilityContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'requiredCapabilities' in value &&
    typeof (value as RequiredCapabilityContributor).requiredCapabilities === 'function'
  );
}
