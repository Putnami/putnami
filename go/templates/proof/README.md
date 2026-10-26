# @putnami/go-templates-proof

Proves that every Go template in this directory renders into a project that vets, builds, passes its own tests and passes the linters `putnami lint` runs, against this repository's framework modules. The lint step runs golangci-lint with the Go extension's `config/.golangci.yml`, then staticcheck, at the versions `go/extension/tools/versions.json` pins. A missing linter fails the proof: run `putnami install` to install them.

Each template declares its own feature in its `putnami.features.json`, with `tooling/project-scaffolding` as parent. Each step is a top-level test bound with `spectest.Proves` to the check the template declares for it: go vet and go build, go test, then the linters. When a session runs the proof's tests, the spec gate joins those observations to the template's requirements. The proof declares each template as a dependency, so a session that validates a template without running the proof reads them from the proof's cached test result. A template added beside the proof fails `TestEveryGoTemplateDeclaresTheChecksThisProofObserves` until it is listed in `provenTemplates` and declares the three checks; a check no test observes then fails the template's spec gate as check-not-observed, because `TestEveryGoTemplateEnforcesItsSpec` requires each template to enforce its spec. This project owns no feature: its `featureAuthority` names `tooling/project-scaffolding`.

## Commands

```bash
putnami test @putnami/go-templates-proof  # render and run every template
```

## Documentation

`doc.go` and `proof_test.go` describe each step. The design is recorded in [ADR 0006](../../../tooling/scaffold/doc/adr/0006-templates-run-against-the-workspace-framework.md). The templates themselves are documented by the README each one renders: [go-library](../go-library/README.md.template) and [go-server](../go-server/README.md.template).
