/**
 * Stable, machine-readable codes carried by every {@link DocumentError}.
 *
 * Consumers should branch on `error.code` rather than the error class name or
 * message, since codes are part of the package's public contract and survive
 * minification. Each code maps to a specific failure condition described on the
 * error class that raises it.
 */
export const DocumentErrorCode = {
  /** A write was attempted with an empty document body. */
  EmptyDocument: 'EMPTY_DOCUMENT',
  /** A bulk write was attempted with an empty array. */
  EmptyDocuments: 'EMPTY_DOCUMENTS',
  /** A document id could not be resolved from the supplied value. */
  MissingDocumentId: 'MISSING_DOCUMENT_ID',
  /** A document failed schema validation before being written. */
  ValidationError: 'VALIDATION_ERROR',
  /** A write was accepted by the adapter but did not persist. */
  SaveFailed: 'SAVE_FAILED',
  /** The adapter raised an error while persisting a write. */
  SaveError: 'SAVE_ERROR',
  /** `deleteMany` was called with no filters (refused for safety). */
  DeleteWithoutFilters: 'DELETE_WITHOUT_FILTERS',
  /** Transactions are unavailable or were used incorrectly. See {@link TransactionNotSupported}. */
  TransactionNotSupported: 'TRANSACTION_NOT_SUPPORTED',
  /** A query needs a backend index that has not been declared. See {@link IndexMissing}. */
  IndexMissing: 'INDEX_MISSING',
  /** The backend cannot satisfy a strong-consistency read. See {@link StrongConsistencyUnsupported}. */
  StrongConsistencyUnsupported: 'STRONG_CONSISTENCY_UNSUPPORTED',
  /** A transaction spanned more than one named store. */
  CrossStoreTransaction: 'CROSS_STORE_TRANSACTION',
  /** The backend does not support composite document ids. See {@link CompositeDocumentIdNotSupported}. */
  CompositeIdNotSupported: 'COMPOSITE_ID_NOT_SUPPORTED',
  /** A backend's optional peer dependency is not installed. */
  AdapterNotInstalled: 'ADAPTER_NOT_INSTALLED',
  /** Backend credentials are missing or malformed. */
  InvalidCredentials: 'INVALID_CREDENTIALS',
  /** A pagination cursor could not be decoded or no longer resolves. */
  InvalidCursor: 'INVALID_CURSOR',
  /** A query shape is not supported by the backend. */
  QueryNotSupported: 'QUERY_NOT_SUPPORTED',
  /** An unclassified document-store error. Default code for {@link DocumentError}. */
  Unknown: 'UNKNOWN',
} as const;

export type DocumentErrorCode = (typeof DocumentErrorCode)[keyof typeof DocumentErrorCode];

/**
 * Base class for every error thrown by the document store.
 *
 * Carries a stable {@link DocumentErrorCode} on `code` (defaulting to
 * {@link DocumentErrorCode.Unknown}) and an optional underlying `cause`. Catch
 * this type to handle any document-store failure, and branch on `code` to
 * distinguish specific conditions. All subclasses set a more specific `code`.
 */
export class DocumentError extends Error {
  constructor(
    message: string,
    /** Stable machine-readable code; branch on this rather than the message. */
    public code: DocumentErrorCode = DocumentErrorCode.Unknown,
    /** The lower-level error that triggered this one, when available. */
    public override cause?: Error,
  ) {
    super(message);
    this.name = 'DocumentError';
    if (Error.captureStackTrace) {
      Error.captureStackTrace(this, DocumentError);
    }
  }
}

/**
 * Thrown when a transaction cannot be used as requested.
 *
 * Raised when the backend has no transaction support, when a transaction spans
 * more than one named store, or when a transaction has timed out. The specific
 * situation is distinguished by `code` ({@link DocumentErrorCode.TransactionNotSupported}
 * or {@link DocumentErrorCode.CrossStoreTransaction}); `reason` carries a short
 * machine-friendly tag (e.g. `"capability-missing"`, `"timeout"`).
 */
export class TransactionNotSupported extends DocumentError {
  constructor(
    message: string,
    /** Short machine-friendly tag for the cause, e.g. `"timeout"`. */
    public reason?: string,
    cause?: Error,
    code: DocumentErrorCode = DocumentErrorCode.TransactionNotSupported,
  ) {
    super(message, code, cause);
    this.name = 'TransactionNotSupported';
  }
}

/**
 * Thrown when a query requires a backend index that has not been declared.
 *
 * Only raised when `strictIndexes` is enabled. `code` is
 * {@link DocumentErrorCode.IndexMissing} and `fields` lists the backend field
 * names that need to be covered by a declared index.
 */
export class IndexMissing extends DocumentError {
  constructor(
    message: string,
    /** Backend field names the query filtered/ordered on that need an index. */
    public fields: string[] = [],
    cause?: Error,
  ) {
    super(message, DocumentErrorCode.IndexMissing, cause);
    this.name = 'IndexMissing';
  }
}

/**
 * Thrown when a strong-consistency read is requested from a backend that cannot
 * guarantee it.
 *
 * `code` is {@link DocumentErrorCode.StrongConsistencyUnsupported}. Retry the
 * read with `consistency: 'eventual'` if eventual consistency is acceptable.
 */
export class StrongConsistencyUnsupported extends DocumentError {
  constructor(message: string, cause?: Error) {
    super(message, DocumentErrorCode.StrongConsistencyUnsupported, cause);
    this.name = 'StrongConsistencyUnsupported';
  }
}

/**
 * Thrown when a collection declares a composite document id but the resolved
 * backend does not support composite ids (e.g. Firestore).
 *
 * `code` is {@link DocumentErrorCode.CompositeIdNotSupported}. Use a single-field
 * document id, or a backend that supports composite ids (e.g. the in-memory
 * adapter).
 */
export class CompositeDocumentIdNotSupported extends DocumentError {
  constructor(message: string, cause?: Error) {
    super(message, DocumentErrorCode.CompositeIdNotSupported, cause);
    this.name = 'CompositeDocumentIdNotSupported';
  }
}
