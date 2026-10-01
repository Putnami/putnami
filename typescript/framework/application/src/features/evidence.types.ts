import type { ContributionReference, LocationRoot } from '../capabilities/manifest.types';

/** Closed maturity ladder shared with go.putnami.dev/protocol/features. */
export type MaturityStage =
  | 'modeled'
  | 'coded'
  | 'wired'
  | 'default-on'
  | 'live-verified'
  | 'design-partner-proven'
  | 'ga';

export type EvidenceKind = 'capability' | 'artifact' | 'attestation';
export type EvidenceOutcome = 'supports' | 'contradicts';
export type IssuerKind = 'framework' | 'build' | 'test' | 'delivery' | 'runtime' | 'human';
export type AttestationClaim = 'adoption' | 'support' | 'availability' | 'customer-proof' | string;

export interface FeatureEvidenceDocument {
  $schema?: string;
  protocolVersion: 1;
  evidence: FeatureEvidenceRecord[];
}

export interface FeatureEvidenceRecord {
  id: string;
  feature: string;
  requirement: string;
  stage: MaturityStage;
  outcome: EvidenceOutcome;
  issuer: EvidenceIssuer;
  source: EvidenceSourceSelector;
  subject: EvidenceSubject;
  provenance: EvidenceProvenance;
  persistent?: boolean;
  observedAt?: string;
  observedRepositoryRevision?: string;
}

export interface EvidenceIssuer {
  kind: IssuerKind;
  id: string;
}

export interface EvidenceSourceSelector {
  root: LocationRoot;
  ownerProject?: string;
  package?: string;
  version?: string;
  binding: string;
  environment?: string;
}

export type EvidenceSubject =
  | { kind: 'capability'; contribution: ContributionReference; artifact?: never; attestation?: never }
  | { kind: 'artifact'; contribution?: never; artifact: ArtifactSubject; attestation?: never }
  | { kind: 'attestation'; contribution?: never; artifact?: never; attestation: AttestationSubject };

export interface ArtifactSubject {
  path: string;
  digest: string;
}

export interface AttestationSubject {
  claim: AttestationClaim;
}

export interface EvidenceProvenance {
  root: LocationRoot;
  path: string;
  symbol?: string;
}
