import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { Collection, DocumentId, Field } from '../src/collection';
import { buildDocumentInfraManifest, INFRA_PROTOCOL_VERSION, INFRA_SCHEMA_URL } from '../src/infra';

const users = Collection('users', { id: DocumentId(String), name: Field(String) });
const orders = Collection('orders', { id: DocumentId(String) });
const events = Collection('events', { id: DocumentId(String) }, { db: 'analytics' });

const firestore = () => 'firestore';
const memory = () => 'memory';

describe('buildDocumentInfraManifest', () => {
  specTest(
    'emits nothing for zero collections',
    {
      feature: 'typescript/document-repository',
      requirement: 'infra-publication',
      check: 'zero-collections-emit-nothing',
    },
    () => {
      expect(buildDocumentInfraManifest([], firestore)).toBeNull();
    },
  );

  specTest(
    'emits nothing for the in-memory backend',
    { feature: 'typescript/document-repository', requirement: 'infra-publication', check: 'a-memory-store-is-omitted' },
    () => {
      expect(buildDocumentInfraManifest([users, orders], memory)).toBeNull();
    },
  );

  specTest(
    'emits a single firestore database for one collection',
    {
      feature: 'typescript/document-repository',
      requirement: 'infra-publication',
      check: 'one-collection-emits-one-firestore-database',
    },
    () => {
      expect(buildDocumentInfraManifest([users], firestore)).toEqual({
        $schema: INFRA_SCHEMA_URL,
        protocolVersion: INFRA_PROTOCOL_VERSION,
        databases: [{ name: 'default', engine: 'firestore', schemas: ['users'] }],
      });
    },
  );

  specTest(
    'groups multiple collections in the default store into one database',
    {
      feature: 'typescript/document-repository',
      requirement: 'infra-publication',
      check: 'collections-in-the-default-store-group-into-one-database',
    },
    () => {
      const manifest = buildDocumentInfraManifest([orders, users], firestore);
      expect(manifest?.databases).toEqual([{ name: 'default', engine: 'firestore', schemas: ['orders', 'users'] }]);
    },
  );

  specTest(
    'emits one database per named store, sorted by store name',
    {
      feature: 'typescript/document-repository',
      requirement: 'infra-publication',
      check: 'one-database-per-named-store-sorted-by-name',
    },
    () => {
      const manifest = buildDocumentInfraManifest([users, events], firestore);
      expect(manifest?.databases).toEqual([
        { name: 'analytics', engine: 'firestore', schemas: ['events'] },
        { name: 'default', engine: 'firestore', schemas: ['users'] },
      ]);
    },
  );

  specTest(
    'de-duplicates repeated collection names within a store',
    {
      feature: 'typescript/document-repository',
      requirement: 'infra-publication',
      check: 'repeated-collection-names-are-de-duplicated',
    },
    () => {
      const manifest = buildDocumentInfraManifest([users, users], firestore);
      expect(manifest?.databases).toEqual([{ name: 'default', engine: 'firestore', schemas: ['users'] }]);
    },
  );

  specTest(
    'resolves each store backend independently',
    {
      feature: 'typescript/document-repository',
      requirement: 'infra-publication',
      check: 'each-store-backend-resolves-independently',
    },
    () => {
      // Default store is in-memory; only the firestore-backed named store emits.
      const resolve = (store: string) => (store === 'analytics' ? 'firestore' : 'memory');
      const manifest = buildDocumentInfraManifest([users, orders, events], resolve);
      expect(manifest?.databases).toEqual([{ name: 'analytics', engine: 'firestore', schemas: ['events'] }]);
    },
  );

  specTest(
    'omits a named store that is not configured for firestore',
    {
      feature: 'typescript/document-repository',
      requirement: 'infra-publication',
      check: 'a-store-not-configured-for-firestore-is-omitted',
    },
    () => {
      // Default store is firestore, but the unconfigured named store stays memory.
      const resolve = (store: string) => (store === 'default' ? 'firestore' : 'memory');
      const manifest = buildDocumentInfraManifest([users, events], resolve);
      expect(manifest?.databases).toEqual([{ name: 'default', engine: 'firestore', schemas: ['users'] }]);
    },
  );
});
