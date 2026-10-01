import { Config, Default, Int, Optional } from '@putnami/runtime';

export const DocumentConfig = Config('document', {
  /** Backend type: 'memory' or 'firestore'. */
  backend: Default(String, 'memory'),
  /** Firestore project id. */
  projectId: Optional(String),
  /** Firestore database id. */
  databaseId: Default(String, '(default)'),
  /** Firestore emulator host, e.g. 127.0.0.1:8080. */
  emulatorHost: Optional(String),
  /** Firestore credentials as JSON or a key file path. */
  credentials: Optional(String),
  /**
   * Enforce declared indexes for all adapters. When unset, defaults to
   * true for the firestore backend (where an uncovered query fails only
   * at runtime with FAILED_PRECONDITION) and false for memory.
   */
  strictIndexes: Optional(Boolean),
  /** Operations slower than this threshold (ms) are logged at warn level. 0 disables. */
  slowOperationThresholdMs: Default(Int, 0),
});
