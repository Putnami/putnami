import { describe, expect } from 'bun:test';
import { Optional } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { Collection, DocumentId, Field } from '../src/collection';
import {
  isCollectionDefinition,
  isDocumentIdDefinition,
  isFieldDefinition,
} from '../src/collection/collection-definition';

describe('Collection builders', () => {
  specTest(
    'creates field and document id definitions',
    {
      feature: 'typescript/document-repository',
      requirement: 'collection-contract',
      check: 'field-and-document-id-definitions-are-produced',
    },
    () => {
      const id = DocumentId(String);
      const email = Field(String, { fieldName: 'email_address' });

      expect(isDocumentIdDefinition(id)).toBe(true);
      expect(isFieldDefinition(email)).toBe(true);
      expect(id.options).toEqual({});
      expect(email.options.fieldName).toBe('email_address');
    },
  );

  specTest(
    'creates collection definitions with options',
    {
      feature: 'typescript/document-repository',
      requirement: 'collection-contract',
      check: 'a-collection-definition-carries-its-options',
    },
    () => {
      const collection = Collection(
        'users',
        {
          id: DocumentId(String),
          email: Field(String),
          nickname: Field(Optional(String)),
        },
        {
          db: 'auth',
          indexes: [{ fields: ['email'] }],
        },
      );

      expect(isCollectionDefinition(collection)).toBe(true);
      expect(collection.collectionName).toBe('users');
      expect(collection.options.db).toBe('auth');
      expect(collection.options.indexes).toEqual([{ fields: ['email'] }]);
    },
  );
});
