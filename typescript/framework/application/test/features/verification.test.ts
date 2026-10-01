import { describe, expect, it } from 'bun:test';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { serializeFeatureVerificationReport, validateFeatureVerificationReport } from '../../src/features/verification';
import type {
  FeatureVerificationReport,
  ObservationMeasurement,
  ObservationProvenance,
  ObservedWindow,
} from '../../src/features/verification.types';

const PROTOCOL_DIR = join(import.meta.dir, '../../../../../protocols/features');
const EQUIVALENCE_FIXTURE = join(PROTOCOL_DIR, 'fixtures/equivalence/go-typescript-verification.golden.json');
const INVALID_DIR = join(PROTOCOL_DIR, 'fixtures/invalid');

function read(): FeatureVerificationReport {
  return JSON.parse(readFileSync(EQUIVALENCE_FIXTURE, 'utf8')) as FeatureVerificationReport;
}

/** The golden's measured observation, narrowed so a test can mutate it in place. */
function firstMeasured(report: FeatureVerificationReport): {
  measurement: ObservationMeasurement;
  window: ObservedWindow;
  provenance: ObservationProvenance;
} {
  const measured = report.observations.find(({ measurement }) => measurement !== undefined);
  if (!measured?.measurement || !measured.window) throw new Error('the golden must carry a measured observation');
  return { measurement: measured.measurement, window: measured.window, provenance: measured.provenance };
}

describe('feature verification report protocol', () => {
  it('validates and reproduces the shared Go/TypeScript bytes', () => {
    const bytes = readFileSync(EQUIVALENCE_FIXTURE, 'utf8');
    const report = read();
    expect(validateFeatureVerificationReport(report)).toEqual([]);
    expect(serializeFeatureVerificationReport(report)).toBe(bytes);

    report.observations.reverse();
    expect(serializeFeatureVerificationReport(report)).toBe(bytes);
  });

  it('rejects every report the Go reader rejects', () => {
    const fixtures = readdirSync(INVALID_DIR).filter((name) => name.startsWith('verification-'));
    expect(fixtures.length).toBeGreaterThan(0);

    for (const name of fixtures) {
      const report = JSON.parse(readFileSync(join(INVALID_DIR, name), 'utf8')) as FeatureVerificationReport;
      const diagnostics = validateFeatureVerificationReport(report);
      expect(diagnostics.length, `${name} was accepted by the TypeScript reader`).toBeGreaterThan(0);
    }
  });

  it('closes the wire at every level so a producer cannot declare its own authority', () => {
    // The payload must not name an issuer, project, source binding, threshold,
    // or verdict: core derives those from the task and the authored criterion.
    const issuer = JSON.parse(
      readFileSync(join(INVALID_DIR, 'verification-self-declared-issuer.json'), 'utf8'),
    ) as FeatureVerificationReport;
    expect(validateFeatureVerificationReport(issuer).map(({ code, field }) => `${code}|${field}`)).toContain(
      'features.unknown_field|observations[0].issuer',
    );

    const report = read();
    const measured = firstMeasured(report);
    Object.assign(report, { verdict: 'verified' });
    Object.assign(measured.provenance, { root: 'workspace' });
    Object.assign(measured.measurement, { threshold: 25 });
    Object.assign(measured.window, { rolling: true });

    const signature = validateFeatureVerificationReport(report).map(({ code, field }) => `${code}|${field}`);
    expect(signature).toContain('features.unknown_field|verdict');
    expect(signature).toContain('features.unknown_field|observations[0].provenance.root');
    expect(signature).toContain('features.unknown_field|observations[0].measurement.threshold');
    expect(signature).toContain('features.unknown_field|observations[0].window.rolling');
  });

  it('refuses an observation that states both a verdict and a measurement', () => {
    const report = read();
    const measured = report.observations.find(({ measurement }) => measurement !== undefined);
    if (!measured) throw new Error('missing measured fixture');
    Object.assign(measured, { status: 'passed' });

    expect(validateFeatureVerificationReport(report).map(({ code, field }) => `${code}|${field}`)).toContain(
      'features.invalid_observation|observations[0]',
    );
  });

  it('rejects a non-finite measurement, an escaping provenance path, and a duplicated check', () => {
    const report = read();
    const measured = firstMeasured(report);
    measured.measurement.value = Number.POSITIVE_INFINITY;
    measured.provenance.path = '../outside/delivery.go';
    const [duplicated] = report.observations.slice(2);
    if (!duplicated) throw new Error('the golden must carry an acceptance observation');
    report.observations.push({ ...duplicated });

    const signature = validateFeatureVerificationReport(report).map(({ code, field }) => `${code}|${field}`);
    expect(signature).toContain('features.invalid_observation|observations[0].measurement.value');
    expect(signature).toContain('features.path_escape|observations[0].provenance.path');
    expect(signature).toContain('features.duplicate_observation|observations[3]');
    expect(validateFeatureVerificationReport(report).map(({ code, field }) => `${code}|${field}`)).toEqual(signature);
  });
});
