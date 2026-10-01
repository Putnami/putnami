import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { buildEventsInfraManifest } from '../src/infra/requirements';

// EQUIVALENCE_FIXTURE_PATH is the shared golden file the Go events producer
// asserts against too. Both languages writing identical bytes to that fixture
// is the test surface the audit's S1 finding would have caught (a silent shape
// divergence between the two implementations).
const EQUIVALENCE_FIXTURE_PATH = join(__dirname, '../../../../protocols/infra/fixtures/equivalence/events.golden.json');

describe('cross-language equivalence', () => {
  // Counterpart: go/framework/events/cross_language_test.go.
  // Same conceptual inputs, same golden output. A change to either
  // implementation that breaks byte-equivalence fails both tests.
  specTest(
    'events producer matches the shared golden fixture',
    {
      feature: 'typescript/event-messaging',
      requirement: 'cross-language-records',
      check: 'the-producer-output-matches-the-shared-golden-fixture',
    },
    async () => {
      // The publish list is unsorted with a duplicate so the dedupe-and-sort
      // path is exercised; the subscribe list is a single distinct topic.
      const manifest = buildEventsInfraManifest(
        ['order.shipped', 'order.created', 'order.created'],
        ['payment.settled'],
      );
      const got = `${JSON.stringify(manifest, null, 2)}\n`;

      const want = await readFile(EQUIVALENCE_FIXTURE_PATH, 'utf8');

      expect(got).toBe(want);
    },
  );
});
