import type { Discoverer, InfraRequirement, SchemaContribution } from './manifest.types';

/** Static contribution inventory exposed by framework/package adapters. */
export interface CapabilityInventory {
  schemas?: Omit<SchemaContribution, 'provenance'>[];
  discoverers?: Omit<Discoverer, 'provenance'>[];
  infraRequirements?: Omit<InfraRequirement, 'provenance'>[];
}

/**
 * Plugin-side adapter for inventory that is not represented by a lifecycle
 * interface. Provenance remains emitter-owned and is resolved from the stable
 * scheduler package graph.
 */
export interface CapabilityInventoryContributor {
  capabilityInventory(): CapabilityInventory;
}

export function isCapabilityInventoryContributor(value: unknown): value is CapabilityInventoryContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'capabilityInventory' in value &&
    typeof (value as CapabilityInventoryContributor).capabilityInventory === 'function'
  );
}
