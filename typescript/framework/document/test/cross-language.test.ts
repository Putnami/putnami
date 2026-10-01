import { describe, expect } from 'bun:test';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { Collection, DocumentId, Field } from '../src/collection';
import { buildDocumentInfraManifest } from '../src/infra';

// EQUIVALENCE_FIXTURE_PATH is the shared golden fixture for the document
// producer. Document is TypeScript-only today, so there is no Go counterpart;
// the golden still pins the wire shape so a future Go document producer is
// constrained to byte-equivalence (see the fixture directory README).
const EQUIVALENCE_FIXTURE_PATH = join(
  __dirname,
  '../../../../protocols/infra/fixtures/equivalence/document.golden.json',
);

describe('cross-language equivalence', () => {
  specTest(
    'document producer matches the shared golden fixture',
    {
      feature: 'typescript/document-repository',
      requirement: 'infra-publication',
      check: 'the-producer-output-matches-the-shared-golden-fixture',
    },
    async () => {
      const users = Collection('users', { id: DocumentId(String), name: Field(String) });
      const orders = Collection('orders', { id: DocumentId(String) });
      const events = Collection('events', { id: DocumentId(String) }, { db: 'analytics' });

      // The default store groups orders + users (collection names sorted); the
      // named "analytics" store holds events. Every store resolves to firestore,
      // and stores are emitted sorted by name.
      const manifest = buildDocumentInfraManifest([users, orders, events], () => 'firestore');
      const got = `${JSON.stringify(manifest, null, 2)}\n`;

      const want = await readFile(EQUIVALENCE_FIXTURE_PATH, 'utf8');

      expect(got).toBe(want);
    },
  );
});
