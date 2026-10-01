import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { parseCredentials } from '../src/adapter/firestore.adapter';
import { DocumentError, DocumentErrorCode } from '../src/errors';

describe('parseCredentials', () => {
  specTest(
    'returns an empty config when no credentials are set',
    {
      feature: 'typescript/document-repository',
      requirement: 'credential-redaction',
      check: 'no-credentials-yields-an-empty-config',
    },
    () => {
      expect(parseCredentials(undefined)).toEqual({});
      expect(parseCredentials('')).toEqual({});
    },
  );

  specTest(
    'parses inline JSON credentials',
    {
      feature: 'typescript/document-repository',
      requirement: 'credential-redaction',
      check: 'inline-json-credentials-are-parsed',
    },
    () => {
      const json = JSON.stringify({ project_id: 'p', client_email: 'a@b.c', private_key: 'k' });
      expect(parseCredentials(json)).toEqual({
        credentials: { project_id: 'p', client_email: 'a@b.c', private_key: 'k' },
      });
    },
  );

  specTest(
    'treats a non-JSON value as a key file path',
    {
      feature: 'typescript/document-repository',
      requirement: 'credential-redaction',
      check: 'a-non-json-value-is-treated-as-a-key-file-path',
    },
    () => {
      expect(parseCredentials('/secrets/key.json')).toEqual({ keyFilename: '/secrets/key.json' });
    },
  );

  specTest(
    'throws a typed DocumentError (not a raw SyntaxError) on malformed JSON',
    {
      feature: 'typescript/document-repository',
      requirement: 'credential-redaction',
      check: 'malformed-inline-credentials-raise-a-typed-error',
    },
    () => {
      let thrown: unknown;
      try {
        parseCredentials('{ not valid json');
      } catch (error) {
        thrown = error;
      }

      expect(thrown).toBeInstanceOf(DocumentError);
      expect(thrown).not.toBeInstanceOf(SyntaxError);
      expect((thrown as DocumentError).code).toBe(DocumentErrorCode.InvalidCredentials);
      // Preserves the underlying parse error as cause.
      expect((thrown as DocumentError).cause).toBeInstanceOf(SyntaxError);
    },
  );

  specTest(
    'never echoes the secret credential value in the error message',
    {
      feature: 'typescript/document-repository',
      requirement: 'credential-redaction',
      check: 'the-credential-value-never-appears-in-the-error',
    },
    () => {
      const secret = '{"private_key":"SUPER_SECRET","oops"';
      let thrown: unknown;
      try {
        parseCredentials(secret);
      } catch (error) {
        thrown = error;
      }

      expect(thrown).toBeInstanceOf(DocumentError);
      expect((thrown as DocumentError).message).not.toContain('SUPER_SECRET');
      expect((thrown as DocumentError).message).toContain('document.credentials');
    },
  );
});
