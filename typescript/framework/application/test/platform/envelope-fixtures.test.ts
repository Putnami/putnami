import { describe, expect, it } from 'bun:test';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { parseAndValidateEnvelope } from '../../src/platform';

// Cross-language envelope conformance: the TS platform protocol port must
// accept/reject exactly the envelopes the shared fixture corpus pins,
// against the SAME files Go's protocols/platform/conformance_test.go
// (TestConformance_EnvelopeFixtures, ~line 217) reads. Consuming the
// shared fixtures — instead of re-stating envelope constants in the test —
// means envelope drift between the Go reference and this runtime fails CI
// in both. Do not edit the fixtures here; they are frozen wire shape.
//
// Path mirrors test/capabilities/cross-language.test.ts: five levels up
// from test/platform/ reaches the repo root, then into protocols/.
const FIXTURE_DIR = join(__dirname, '../../../../../protocols/platform/fixtures/envelope');

function fixtures(kind: 'valid' | 'invalid'): string[] {
  return readdirSync(join(FIXTURE_DIR, kind))
    .filter((name) => name.endsWith('.json'))
    .sort();
}

const valid = fixtures('valid');
const invalid = fixtures('invalid');

describe('platform envelope fixtures (cross-language conformance)', () => {
  it('finds the shared fixture corpus on disk', () => {
    // Guard against a silently-empty glob (moved/renamed fixtures) turning
    // the it.each suites below into no-ops — the Go twin fails the same way.
    expect(valid.length).toBeGreaterThan(0);
    expect(invalid.length).toBeGreaterThan(0);
  });

  it.each(valid)('valid/%s parses and validates with no diagnostics', (name) => {
    const data = readFileSync(join(FIXTURE_DIR, 'valid', name), 'utf8');
    expect(parseAndValidateEnvelope(data).diagnostics).toEqual([]);
  });

  it.each(invalid)('invalid/%s produces at least one diagnostic', (name) => {
    const data = readFileSync(join(FIXTURE_DIR, 'invalid', name), 'utf8');
    expect(parseAndValidateEnvelope(data).diagnostics.length).toBeGreaterThan(0);
  });
});
