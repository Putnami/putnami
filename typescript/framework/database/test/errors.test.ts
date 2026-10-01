import { describe, expect, it } from 'bun:test';
import { RepositoryError, RepositoryErrorCode, classifyRollbackCause } from '../src/errors';

function pgError(code: string, extra: Record<string, unknown> = {}): Error {
  return Object.assign(new Error(`pg error ${code}`), { code, ...extra });
}

describe('RepositoryError.fromDatabaseError', () => {
  it('maps a unique violation (23505) and captures the constraint', () => {
    const err = RepositoryError.fromDatabaseError('save failed', pgError('23505', { constraint: 'users_email_key' }));
    expect(err.code).toBe(RepositoryErrorCode.UniqueViolation);
    expect(err.pgCode).toBe('23505');
    expect(err.constraint).toBe('users_email_key');
  });

  it('maps a foreign-key violation (23503)', () => {
    const err = RepositoryError.fromDatabaseError('save failed', pgError('23503'));
    expect(err.code).toBe(RepositoryErrorCode.ForeignKeyViolation);
  });

  it('maps a not-null violation (23502) and captures the column', () => {
    const err = RepositoryError.fromDatabaseError('save failed', pgError('23502', { column: 'name' }));
    expect(err.code).toBe(RepositoryErrorCode.NotNullViolation);
    expect(err.column).toBe('name');
  });

  it('captures postgres snake-case metadata fields', () => {
    const err = RepositoryError.fromDatabaseError(
      'save failed',
      pgError('23502', { column_name: 'name', constraint_name: 'users_name_check' }),
    );
    expect(err.column).toBe('name');
    expect(err.constraint).toBe('users_name_check');
  });

  it('maps a check violation (23514)', () => {
    const err = RepositoryError.fromDatabaseError('save failed', pgError('23514'));
    expect(err.code).toBe(RepositoryErrorCode.CheckViolation);
  });

  it('maps a serialization failure (40001)', () => {
    const err = RepositoryError.fromDatabaseError('update failed', pgError('40001'));
    expect(err.code).toBe(RepositoryErrorCode.SerializationFailure);
    expect(err.pgCode).toBe('40001');
  });

  it('maps a deadlock (40P01)', () => {
    const err = RepositoryError.fromDatabaseError('update failed', pgError('40P01'));
    expect(err.code).toBe(RepositoryErrorCode.DeadlockDetected);
    expect(err.pgCode).toBe('40P01');
  });

  it('falls back to SAVE_ERROR for an unmapped postgres code', () => {
    const err = RepositoryError.fromDatabaseError('save failed', pgError('08006'));
    expect(err.code).toBe(RepositoryErrorCode.SaveError);
    expect(err.pgCode).toBe('08006');
  });

  it('preserves postgres metadata from object-shaped thrown values', () => {
    const err = RepositoryError.fromDatabaseError('save failed', {
      code: '23505',
      constraint: 'users_email_key',
      message: 'duplicate key',
    });
    expect(err.code).toBe(RepositoryErrorCode.UniqueViolation);
    expect(err.pgCode).toBe('23505');
    expect(err.constraint).toBe('users_email_key');
    expect(err.cause?.message).toBe('duplicate key');
  });

  it('falls back to SAVE_ERROR for a non-postgres error and preserves the cause', () => {
    const cause = new Error('boom');
    const err = RepositoryError.fromDatabaseError('save failed', cause);
    expect(err.code).toBe(RepositoryErrorCode.SaveError);
    expect(err.pgCode).toBeUndefined();
    expect(err.cause).toBe(cause);
    expect(err.message).toBe('save failed');
  });
});

describe('classifyRollbackCause — secret-free rollback cause classification', () => {
  const SECRET = 'tok_SUPERSECRET_hunter2';

  it('returns "" for a nullish cause (a committed transaction has none)', () => {
    expect(classifyRollbackCause(undefined)).toBe('');
    expect(classifyRollbackCause(null)).toBe('');
  });

  it('classifies a raw driver error to its SQLSTATE, never the message', () => {
    const err = Object.assign(new Error(`duplicate key value: token=(${SECRET})`), { code: '23505' });
    const cause = classifyRollbackCause(err);
    expect(cause).toBe('23505');
    expect(cause).not.toContain(SECRET);
  });

  it('classifies a serialization failure to 40001', () => {
    expect(classifyRollbackCause(Object.assign(new Error('serialize'), { code: '40001' }))).toBe('40001');
  });

  it('classifies a wrapped RepositoryError to its SQLSTATE, never the message', () => {
    const repoErr = RepositoryError.fromDatabaseError(`save failed for ${SECRET}`, {
      code: '23505',
      message: `duplicate: ${SECRET}`,
    });
    const cause = classifyRollbackCause(repoErr);
    expect(cause).toBe('23505');
    expect(cause).not.toContain(SECRET);
  });

  it('classifies a RepositoryError with no pg code to its typed code', () => {
    const repoErr = new RepositoryError(`boom ${SECRET}`, RepositoryErrorCode.ValidationError);
    const cause = classifyRollbackCause(repoErr);
    expect(cause).toBe(RepositoryErrorCode.ValidationError);
    expect(cause).not.toContain(SECRET);
  });

  it('classifies a plain Error to its class name, never the message', () => {
    const cause = classifyRollbackCause(new Error(`insert failed token=${SECRET}`));
    expect(cause).toBe('Error');
    expect(cause).not.toContain(SECRET);
  });

  it('classifies an AggregateError to its class name', () => {
    expect(classifyRollbackCause(new AggregateError([new Error(SECRET)], 'multi'))).toBe('AggregateError');
  });

  it('classifies a non-error value to "unknown"', () => {
    expect(classifyRollbackCause(SECRET)).toBe('unknown');
    expect(classifyRollbackCause(42)).toBe('unknown');
  });
});
