/**
 * Run-scoped feature verification report, mirroring
 * go.putnami.dev/protocol/features. It is transport, not authority: it declares
 * no issuer, project, source binding, threshold, or objective verdict, so a
 * producer can never choose both its target and its result.
 */
export interface FeatureVerificationReport {
  $schema?: string;
  protocolVersion: 1;
  observations: FeatureVerificationObservation[];
}

export type ObservationStatus = 'passed' | 'failed' | 'skipped';

export type VerificationAggregation =
  | 'value'
  | 'count'
  | 'sum'
  | 'avg'
  | 'min'
  | 'max'
  | 'p50'
  | 'p95'
  | 'p99'
  | 'ratio';

/** An observation carries either an acceptance status or a measured window, never both. */
export type FeatureVerificationObservation = ObservationIdentity &
  (
    | {
        status: ObservationStatus;
        measurement?: never;
        window?: never;
        environment?: never;
      }
    | {
        status?: never;
        measurement: ObservationMeasurement;
        window: ObservedWindow;
        environment?: string;
      }
  );

export interface ObservationIdentity {
  feature: string;
  requirement: string;
  check: string;
  provenance: ObservationProvenance;
}

export interface ObservationMeasurement {
  name: string;
  aggregation: VerificationAggregation;
  value: number;
  unit: string;
}

export interface ObservedWindow {
  start: string;
  end: string;
}

/**
 * Resolved against the reporting task's own project root; a report never names
 * a root of its own.
 */
export interface ObservationProvenance {
  path: string;
  symbol?: string;
}
