import { describe, expect } from 'bun:test';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { Config } from '../../src/config/config';
import { buildInfraRequirements } from '../../src/config/infra-requirements';
import { Sensitive } from '../../src/schema';
import { specTest } from '../../src/spectest';

// GOLDEN proves a requirement of the infra protocol's feature under a check of
// this package's own.
const GOLDEN = {
  feature: 'infra/infrastructure-requirements-contract',
  requirement: 'producers-emit-the-shared-goldens',
  check: 'the-typescript-runtime-secrets-producer-emits-its-golden',
};

// EQUIVALENCE_FIXTURE_PATH is the shared golden file the Go secrets producer
// (configextract) asserts against too. Both languages writing identical bytes
// to that fixture is the test surface the audit's S1 finding would have caught
// (a silent shape divergence between the two implementations).
const EQUIVALENCE_FIXTURE_PATH = join(
  __dirname,
  '../../../../../protocols/infra/fixtures/equivalence/secrets.golden.json',
);

describe('cross-language equivalence', () => {
  // Counterpart: go/extension/internal/jobs/configextract/cross_language_test.go.
  // Same conceptual inputs, same golden output. A change to either
  // implementation that breaks byte-equivalence fails both tests.
  specTest('secrets producer matches the shared golden fixture', GOLDEN, async () => {
    // A top-level "database.password" and a nested camelCase
    // "integrations.stripe.apiKey", each named by its config key; names are
    // emitted sorted.
    const database = Config('database', { password: Sensitive(String) });
    const integrations = Config('integrations', { stripe: { publishable: String, apiKey: Sensitive(String) } });

    const manifest = buildInfraRequirements([database, integrations]);
    const got = `${JSON.stringify(manifest, null, 2)}\n`;

    const want = await readFile(EQUIVALENCE_FIXTURE_PATH, 'utf8');

    expect(got).toBe(want);
  });
});
