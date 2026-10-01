import { registerHealthConformanceTests } from '../../src/conformance';

// The liveness / readiness / version health conformance pack runs in
// @putnami/application's own suite through the exported pack runner, the same
// one-line opt-in a downstream project uses. The machinery lives in
// src/conformance/health-runner.ts; the Go counterpart is
// go.putnami.dev/app/conformance. Pure pack — no external service — so it always
// runs in the unit gate.
registerHealthConformanceTests();
