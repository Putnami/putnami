# @putnami/go-templates-proof

Proves that every Go template in this directory renders into a project that vets, builds, passes its own tests and passes the linters `putnami lint` runs, against this repository's framework modules. The lint step runs golangci-lint with the Go extension's `config/.golangci.yml`, then staticcheck, at the versions `go/extension/tools/versions.json` pins. A missing linter fails the proof: run `putnami install` to install them.

## Commands

```bash
putnami test @putnami/go-templates-proof  # render and run every template
```

## Documentation

`doc.go` and `proof_test.go` describe each step. The design is recorded in [ADR 0006](../../../tooling/scaffold/doc/adr/0006-templates-run-against-the-workspace-framework.md). The templates themselves are documented by the README each one renders: [go-library](../go-library/README.md.template) and [go-server](../go-server/README.md.template).
