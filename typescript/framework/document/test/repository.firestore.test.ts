import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import type { DocumentAdapter } from '../src/adapter/document.adapter';
import { closeAllBackends } from '../src/factory';
import { Collection, type CollectionDefinition, DocumentId, Field } from '../src/collection';
import { CompositeDocumentIdNotSupported, IndexMissing } from '../src/errors';
import { Repository } from '../src/repository/repository';
import { createFirestoreAdapter } from './firestore.fake';

const firestoreAvailable = await import('@google-cloud/firestore').then(() => true).catch(() => false);
const emulatorHost = process.env.FIRESTORE_EMULATOR_HOST;

function configureFirestore(extra: Record<string, unknown> = {}) {
  resetConfigLoader();
  process.env.CONFIG_DATA = JSON.stringify({
    document: {
      firestore: {
        backend: 'firestore',
        projectId: 'putnami-document-tests',
        ...(emulatorHost ? { emulatorHost } : {}),
        ...extra,
      },
    },
  });
}

/**
 * Builds a repository whose backend resolves to a real {@link FirestoreAdapter}
 * driven by the in-memory fake client, bypassing the optional
 * `@google-cloud/firestore` peer dependency. The repository's divergent
 * Firestore semantics (strict-index default, strong-consistency capability,
 * composite-id rejection) are decided from config + adapter capabilities, so
 * this exercises the *same code path* a live Firestore backend would — without
 * an emulator — while still letting queries that clear every gate run to a real
 * (empty) result instead of failing on the absent client.
 */
function firestoreRepository<T extends CollectionDefinition>(collection: T): Repository<T> {
  const repo = new Repository(collection);
  const adapter: DocumentAdapter = createFirestoreAdapter().adapter;
  (repo as unknown as { _adapter: Promise<DocumentAdapter> })._adapter = Promise.resolve(adapter);
  return repo;
}

/**
 * Firestore's divergent *repository-level* contract (strict-index enforcement
 * default-on, composite-id rejection, strong-consistency acceptance) is decided
 * from config and adapter capabilities, not from a live round-trip, so these
 * cases hold the Firestore backend to its documented semantics without the
 * emulator or the optional `@google-cloud/firestore` peer dependency installed.
 * They run the real {@link FirestoreAdapter} over the in-memory fake.
 *
 * Round-trip behavior that genuinely needs a live backend lives in the
 * emulator-gated describe block below.
 */
describe('Repository (firestore divergent semantics, no emulator required)', () => {
  beforeEach(() => {
    configureFirestore();
  });

  afterEach(async () => {
    await closeAllBackends();
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  it('rejects composite document ids (capability-driven)', async () => {
    const repo = firestoreRepository(
      Collection(
        `memberships_${crypto.randomUUID()}`,
        {
          tenant: DocumentId(String),
          user: DocumentId(String),
          role: Field(String),
        },
        { db: 'firestore' },
      ),
    );

    await expect(repo.save({ tenant: 'acme', user: 'user-1', role: 'admin' })).rejects.toBeInstanceOf(
      CompositeDocumentIdNotSupported,
    );
  });

  it('defaults strict-index enforcement on for the firestore backend', async () => {
    const repo = firestoreRepository(
      Collection(
        `users_${crypto.randomUUID()}`,
        {
          id: DocumentId(String),
          email: Field(String),
          name: Field(String),
        },
        // The index must also cover `id`, which find() appends as the stable
        // pagination tiebreaker.
        { db: 'firestore', indexes: [{ fields: ['email', 'id'] }] },
      ),
    );

    // `name` is not covered by any declared index, so Firestore (strict by
    // default) must reject the query up front rather than failing at runtime.
    await expect(repo.find({ name: 'Alice' })).rejects.toBeInstanceOf(IndexMissing);

    // A covered query clears index validation and runs to a real empty result.
    const covered = await repo.find({ email: 'alice@example.com' });
    expect(covered.items).toEqual([]);
  });

  it('allows opting strict-index enforcement off via config', async () => {
    configureFirestore({ strictIndexes: false });

    const repo = firestoreRepository(
      Collection(
        `users_${crypto.randomUUID()}`,
        {
          id: DocumentId(String),
          email: Field(String),
          name: Field(String),
        },
        { db: 'firestore', indexes: [{ fields: ['email'] }] },
      ),
    );

    // With strict indexes disabled, an uncovered query must not be rejected by
    // index validation — it runs to a real empty result instead.
    const result = await repo.find({ name: 'Alice' });
    expect(result.items).toEqual([]);
  });

  it('does not reject strong-consistency reads (Firestore supports them)', async () => {
    // Strict indexes off so the index gate cannot mask the consistency check.
    configureFirestore({ strictIndexes: false });

    const repo = firestoreRepository(
      Collection(
        `users_${crypto.randomUUID()}`,
        {
          id: DocumentId(String),
          name: Field(String),
        },
        { db: 'firestore' },
      ),
    );

    // Firestore advertises strongConsistency, so a strong read must clear the
    // consistency gate and run to a real result rather than throwing.
    const result = await repo.find({ name: 'Alice' }, { consistency: 'strong' });
    expect(result.items).toEqual([]);
  });

  it('rejects multiple negative filters in a single query', async () => {
    // Disable strict indexes so the adapter's negative-filter check (not the
    // repository index gate) is what rejects the query.
    configureFirestore({ strictIndexes: false });

    const repo = firestoreRepository(
      Collection(
        `users_${crypto.randomUUID()}`,
        {
          id: DocumentId(String),
          email: Field(String),
          name: Field(String),
        },
        { db: 'firestore' },
      ),
    );

    await expect(repo.find({ name: { not: 'blocked' }, email: { not: 'blocked@example.com' } })).rejects.toMatchObject({
      code: 'QUERY_NOT_SUPPORTED',
    });
  });
});

describe.skipIf(!firestoreAvailable || !emulatorHost)('Repository (firestore, emulator)', () => {
  beforeEach(() => {
    configureFirestore();
  });

  afterEach(async () => {
    await closeAllBackends();
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  it('supports basic CRUD against the emulator', async () => {
    const repo = new Repository(
      Collection(
        `users_${crypto.randomUUID()}`,
        {
          id: DocumentId(String),
          name: Field(String),
        },
        // `name` must be index-covered: strict indexes default on for Firestore.
        { db: 'firestore', indexes: [{ fields: ['name'] }] },
      ),
    );

    await repo.save({ id: 'user-1', name: 'Alice' });
    expect(await repo.get('user-1')).toEqual({ id: 'user-1', name: 'Alice' });

    const found = await repo.find({ name: 'Alice' });
    expect(found.items).toEqual([{ id: 'user-1', name: 'Alice' }]);

    expect(await repo.delete('user-1')).toEqual({
      success: true,
      item: { id: 'user-1', name: 'Alice' },
    });
  });
});
