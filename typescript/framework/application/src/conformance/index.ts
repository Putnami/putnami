// @putnami/application/conformance — the exported health-probe conformance
// pack. A downstream test file certifies the framework's liveness /
// readiness / version health contract with a single committed line:
//
//   import { registerHealthConformanceTests } from '@putnami/application/conformance';
//   registerHealthConformanceTests();
//
// See ./health-runner for the machinery and go/framework/app/conformance for the
// Go counterpart and the pack manifest (pack.json). Pure pack: no external
// service, no skip gate.
export { registerHealthConformanceTests } from './health-runner';
