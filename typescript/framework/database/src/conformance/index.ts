// @putnami/database/conformance — the exported cross-language transaction /
// concurrency conformance pack. A downstream test file opts into
// the full corpus with a single committed line:
//
//   import { registerConformanceTests } from '@putnami/database/conformance';
//   registerConformanceTests();
//
// See ./runner for the machinery and protocols/transaction/conformance for the
// single source of truth corpus and its pack manifest (pack.json).
export { registerConformanceTests } from './runner';
