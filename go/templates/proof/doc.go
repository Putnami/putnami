// Package proof holds no code: its test renders every Go template of this
// repository into a throwaway workspace wired to the workspace's own framework
// modules, downloads the third-party modules it compiles, then vets, builds,
// tests it offline and runs the linters `putnami lint` runs on it, at their
// pinned versions and with the Go extension's configuration.
//
// The design is recorded in
// tooling/scaffold/doc/adr/0006-templates-run-against-the-workspace-framework.md.
package proof
