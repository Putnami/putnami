import { describe, expect, it } from 'bun:test';
import { DateIso, Optional } from '@putnami/runtime';
import { Collection, DocumentId, Field } from '../src/collection';
import { documentHelper } from '../src/metadata/document.helper';

describe('DocumentHelper', () => {
  it('maps properties to backend field names', () => {
    const helper = documentHelper(
      Collection('users', {
        id: DocumentId(String),
        fullName: Field(String, { fieldName: 'full_name' }),
      }),
    );

    expect(helper.fieldName('fullName')).toBe('full_name');
    expect(helper.propertyName('full_name')).toBe('fullName');
  });

  it('normalizes scalar document ids', () => {
    const helper = documentHelper(
      Collection('users', {
        id: DocumentId(String),
        name: Field(String),
      }),
    );

    expect(helper.normalizeDocumentId('user-1')).toBe('user-1');
    expect(helper.normalizeDocumentId({ id: 'user-2' })).toBe('user-2');
  });

  it('normalizes composite document ids using backend field names', () => {
    const helper = documentHelper(
      Collection('sessions', {
        accountId: DocumentId(String, { fieldName: 'account_id' }),
        region: DocumentId(String),
        name: Field(String),
      }),
    );

    expect(helper.normalizeDocumentId({ accountId: 'acct-1', region: 'eu' })).toEqual({
      account_id: 'acct-1',
      region: 'eu',
    });
  });

  it('converts documents to raw storage values and back', () => {
    const helper = documentHelper(
      Collection('events', {
        id: DocumentId(String),
        createdAt: Field(DateIso, { fieldName: 'created_at' }),
        name: Field(String),
      }),
    );

    const createdAt = new Date('2026-04-24T12:00:00.000Z');
    const raw = helper.toDocument({
      id: 'evt-1',
      createdAt,
      name: 'Launch',
    });

    expect(raw).toEqual({
      id: 'evt-1',
      created_at: createdAt.toISOString(),
      name: 'Launch',
    });

    expect(helper.toEntity(raw)).toEqual({
      id: 'evt-1',
      createdAt: createdAt.toISOString(),
      name: 'Launch',
    });
  });

  it('passes a stored ISO-string date through unchanged on read', () => {
    const helper = documentHelper(
      Collection('events', {
        id: DocumentId(String),
        createdAt: Field(DateIso, { fieldName: 'created_at' }),
        name: Field(String),
      }),
    );

    // Adapters normally store DateIso fields as ISO strings; reading one back
    // must not re-stringify or otherwise mutate it.
    const stored = { id: 'evt-1', created_at: '2026-04-24T12:00:00.000Z', name: 'Launch' };
    expect(helper.toEntity(stored)).toEqual({
      id: 'evt-1',
      createdAt: '2026-04-24T12:00:00.000Z',
      name: 'Launch',
    });
  });

  it('normalizes a native Date returned by a backend to an ISO string on read', () => {
    const helper = documentHelper(
      Collection('events', {
        id: DocumentId(String),
        createdAt: Field(DateIso, { fieldName: 'created_at' }),
        name: Field(String),
      }),
    );

    // Some backends hydrate a date field as a native Date (e.g. a Firestore
    // Timestamp via .toDate()); the entity-facing DateIso value is an ISO string.
    const createdAt = new Date('2026-04-24T12:00:00.000Z');
    expect(helper.toEntity({ id: 'evt-1', created_at: createdAt, name: 'Launch' })).toEqual({
      id: 'evt-1',
      createdAt: createdAt.toISOString(),
      name: 'Launch',
    });
  });

  it('validates partial and full documents', () => {
    const helper = documentHelper(
      Collection('profiles', {
        id: DocumentId(String),
        email: Field(String),
        bio: Field(Optional(String)),
      }),
    );

    expect(helper.validateDocument({ id: 'p-1' })).toEqual([]);
    expect(helper.validateFullDocument({ id: 'p-1' }).length).toBeGreaterThan(0);
    expect(helper.validateFullDocument({ id: 'p-1', email: 'a@example.com' })).toEqual([]);
  });

  it('rejects unknown fields in full-document validation', () => {
    const helper = documentHelper(
      Collection('profiles', {
        id: DocumentId(String),
        name: Field(String),
        email: Field(String),
      }),
    );

    const errors = helper.validateFullDocument({
      id: 'p-1',
      name: 'Ada',
      emial: 'typo@example.com',
      extra: 1,
    });

    // A full (replace) write drops keys not in the schema; if validation does
    // not flag them, submitted and persisted documents silently diverge.
    expect(errors.some((error) => error.field === 'profiles.emial')).toBe(true);
    expect(errors.some((error) => error.field === 'profiles.extra')).toBe(true);
    // The known, valid fields must not produce errors.
    expect(errors.some((error) => error.field === 'profiles.id')).toBe(false);
    expect(errors.some((error) => error.field === 'profiles.name')).toBe(false);
  });

  it('accepts a valid full document with only known fields', () => {
    const helper = documentHelper(
      Collection('profiles', {
        id: DocumentId(String),
        name: Field(String),
        email: Field(String),
      }),
    );

    expect(helper.validateFullDocument({ id: 'p-1', name: 'Ada', email: 'ada@example.com' })).toEqual([]);
  });

  it('allows a partial update of a subset of known fields', () => {
    const helper = documentHelper(
      Collection('profiles', {
        id: DocumentId(String),
        name: Field(String),
        email: Field(String),
      }),
    );

    // Merge/partial writes legitimately carry a subset of valid keys and must
    // still pass — unknown-field rejection is scoped to the full (replace) path.
    expect(helper.validateDocument({ id: 'p-1', name: 'Ada' })).toEqual([]);
  });
});
