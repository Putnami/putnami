// Package proof holds no code: its tests render every Go template of this
// repository into a throwaway workspace wired to the workspace's own framework
// modules, download the third-party modules it compiles, then vet, build and
// test it offline and run the linters `putnami lint` runs on it, at their
// pinned versions and with the Go extension's configuration. Each step is a
// test bound with spectest to the check the template's feature declares for
// it.
//
// The design is recorded in
// tooling/scaffold/doc/adr/0006-templates-run-against-the-workspace-framework.md.
package proof
