/**
 * Test helpers for `@putnami/runtime`.
 *
 * These utilities are intentionally segregated from the main entrypoint so
 * production code can't accidentally depend on them and inflate the package
 * surface. Import via `@putnami/runtime/testing`.
 */

export { MemoryLogger, MemorySink } from '../logger/memory.logger';
export {
  assertRecord,
  caseLogger,
  caseMessage,
  compareRecord,
  type ConformanceCase,
  dumpRecords,
  findCase,
  findRecord,
  loadCases,
  manifestPath,
  TOKEN_NUMBER,
  TOKEN_STRING,
  TOKEN_TIMESTAMP,
} from './log-conformance';
