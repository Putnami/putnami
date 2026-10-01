# ADR 0001 — Keep Python explicit and experimental

- **Status**: accepted
- **Scope**: `@putnami/python`, `python-server`, `python-library`, and their samples and public documentation

## Context

Putnami ships a Python workspace extension and two templates, but no Python
framework package family. Presenting that surface like the Go and TypeScript
ones would imply default adoption, compatibility and parity commitments that do
not exist.

## Decision

The Python extension, `python-server` and `python-library` are experimental:
explicit opt-in paths, never the default project choice, with no parity promise
with Go or TypeScript. The extension is a thin integration layer around uv,
Ruff, pytest and a configured application entrypoint. The templates are small
FastAPI and setuptools starting points.

The workspace support catalog records the three subjects with
`status: experimental`, `default: false` and `parity: unsupported`. Their
features stay `modeled`: current jobs produce no exact Feature Evidence, and no
manual evidence is written to promote them.

## Enforceable invariants

- Python entry points disclose the experimental, non-default, non-parity
  classification before telling users to create a project.
- Template descriptors and generated project manifests name `@putnami/python`
  explicitly; no template selects it as a workspace default.
- Python install and upgrade act only through the explicitly enabled extension
  and selected workspace Python projects. A missing project produces a visible
  skip or failure, never an invented result.
- Python documentation describes only existing extension and template behavior.
  It claims no Python framework and no Go or TypeScript parity.

## Rejected alternatives

- **Classify Python as preview or stable.** The compatibility, evidence and
  parity guarantees do not support it.
- **Make the templates a default starter path.** Selecting a language
  experiment would become an implicit recommendation.
- **Add framework or parity features now.** This decision is a boundary, not a
  roadmap.
- **Hand-author Feature Evidence.** Documentation and tests are reviewable
  protections, not generated evidence.

## Consequences

- The extension and templates can change incompatibly while experimental;
  users validate upgrades in their own experiments.
- Promotion requires generated evidence and a reviewed support-policy decision.
