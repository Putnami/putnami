import { describe, expect, it } from 'bun:test';
import { StorageError } from '../src/errors';

describe('StorageError', () => {
  it('should store message and code', () => {
    const error = new StorageError('Something went wrong', 'STORAGE_FAIL');
    expect(error.message).toBe('Something went wrong');
    expect(error.code).toBe('STORAGE_FAIL');
    expect(error.name).toBe('StorageError');
  });

  it('should store cause', () => {
    const cause = new Error('root cause');
    const error = new StorageError('Wrapper error', 'WRAPPER', cause);
    expect(error.cause).toBe(cause);
  });

  it('should be an instance of Error', () => {
    const error = new StorageError('test', 'TEST');
    expect(error).toBeInstanceOf(Error);
    expect(error).toBeInstanceOf(StorageError);
  });

  it('should have a stack trace', () => {
    const error = new StorageError('test', 'TEST');
    expect(error.stack).toBeDefined();
  });
});
