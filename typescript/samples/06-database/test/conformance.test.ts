import { registerConformanceTests } from '@putnami/database/conformance';

// One-line pack opt-in: certify @example/06-database's provisioned Postgres
// against the shared cross-language transaction / concurrency conformance corpus
// (the pack whose manifest lives at protocols/transaction/conformance; the Go
// counterpart is go.putnami.dev/database/conformance). The runner provisions
// through DATABASE_TEST_BINDINGS — injected by `putnami test` in auto mode from
// this project's declared infra/requirements.json — so the corpus runs live
// against an auto-provisioned database; with no binding it SKIPS.
registerConformanceTests();
