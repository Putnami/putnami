/**
 * Base error class for storage operations.
 */
export class StorageError extends Error {
  constructor(
    message: string,
    public code: string,
    public override cause?: Error,
  ) {
    super(message);
    this.name = 'StorageError';
    if (Error.captureStackTrace) {
      Error.captureStackTrace(this, StorageError);
    }
  }
}
