/**
 * TypeScript mirror of the contract IR (protocols/contracts/contracts.go).
 *
 * These are the hand-written twin of the Go source-of-truth types the emitter
 * consumes. Field names are camelCase and match the Go json tags so a parsed
 * `contracts.json` document is a `ContractManifest` without transformation. The
 * types are prefixed `Contract` to stay collision-free, and the module is
 * exposed through the `@putnami/application/contracts` subpath export.
 */

/** Canonical contract IR for a single project. */
export interface ContractManifest {
  $schema?: string;
  protocolVersion: number;
  name: string;
  enums?: ContractEnum[];
  unions?: ContractUnion[];
  structs?: ContractStruct[];
  configFields?: ContractConfigField[];
  scopes?: ContractScope[];
  capabilities?: ContractCapability[];
  grants?: ContractGrant[];
  claims?: ContractClaim[];
  principalKinds?: ContractPrincipalKind[];
  discovery?: ContractDiscoveryMetadata;
}

/** Closed value set. */
export interface ContractEnum {
  name: string;
  description?: string;
  values: ContractEnumValue[];
}

/** One member of an enum: `name` is the constant identity, `value` the wire form. */
export interface ContractEnumValue {
  name: string;
  value: string;
  description?: string;
}

/** Tagged (discriminated) union. */
export interface ContractUnion {
  name: string;
  description?: string;
  discriminator: string;
  variants: ContractUnionVariant[];
}

/**
 * One arm of a union. Exactly one of `struct` (a reference to a declared struct)
 * or `fields` (an inline field list) is set.
 */
export interface ContractUnionVariant {
  tag: string;
  description?: string;
  struct?: string;
  fields?: ContractField[];
}

/** Named struct / DTO shape. */
export interface ContractStruct {
  name: string;
  description?: string;
  fields?: ContractField[];
}

/** One field of a struct or an inline union variant. */
export interface ContractField {
  name: string;
  type: string;
  description?: string;
  optional?: boolean;
  repeated?: boolean;
}

/** Configuration field and its typed default. */
export interface ContractConfigField {
  name: string;
  type: string;
  description?: string;
  required?: boolean;
  default?: unknown;
  sensitive?: boolean;
}

/** Security scope. */
export interface ContractScope {
  name: string;
  description?: string;
}

/** Authorization capability drawing on declared scopes. */
export interface ContractCapability {
  name: string;
  description?: string;
  scopes?: string[];
}

/** Permission grant conferring a declared capability. */
export interface ContractGrant {
  name: string;
  description?: string;
  capability?: string;
}

/** Token / principal claim typed from the config vocabulary. */
export interface ContractClaim {
  name: string;
  type: string;
  description?: string;
  required?: boolean;
}

/** Recognized principal kind. */
export interface ContractPrincipalKind {
  name: string;
  description?: string;
}

/** Contract-level discovery metadata. */
export interface ContractDiscoveryMetadata {
  title?: string;
  summary?: string;
  version?: string;
  tags?: string[];
}
