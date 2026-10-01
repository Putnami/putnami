import { registerConformanceTests } from '../src/conformance';

// The cross-language transaction / concurrency conformance corpus
// runs in @putnami/database's own suite through the exported pack runner, the
// same one-line opt-in a downstream project uses. The machinery lives in
// src/conformance/runner.ts; the Go counterpart is
// go.putnami.dev/database/conformance. Live provisioning runs only when
// DATABASE_TEST_BINDINGS is injected, so unit runs pay no database cost.
registerConformanceTests();
