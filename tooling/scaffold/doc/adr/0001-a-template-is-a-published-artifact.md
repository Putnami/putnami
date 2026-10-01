# ADR 0001 — A template is a published artifact, not a directory to copy

- **Status**: accepted
- **Scope**: `@putnami/scaffold` (`tooling/scaffold`)

## Context

`putnami projects create --template typescript-server` has to work in a
workspace that has never seen this repository's layout. A checked-in directory
can be copied by a tool that has the checkout; it cannot be installed,
versioned, upgraded, or pinned by anyone else.

Templates are cross-language. A packager inside the Go or TypeScript extension
would make one language's toolchain a prerequisite for scaffolding in another.

## Decision

Templates are packaged, versioned, indexed and published like every other
artifact, by an extension of their own.

`@putnami/scaffold` activates on `putnami.template.json` and produces one
`<name>-<version>.tar.gz` per template. The archive holds the template
manifest, stamped with the resolved version, plus the template's content. It
excludes the template project's own `putnami.json`, which makes the directory a
project *in this workspace* and means nothing in a scaffolded one. The version
is the invocation's version when there is one, otherwise the workspace version;
`--stable` strips the pre-release suffix.

The packager knows nothing about the content. It stages, stamps and archives.
Which files are templates, what placeholders mean and how they render is
`@putnami/cli` behavior that the template manifest protocol does not describe.

The task is the sole owner of `<command-output>/archives` and of the sibling
`metadata.json`, the archive publication manifest the uploader reads. The Go
archive packager owns the same subpath in its manifest; the two never meet,
because a template project is selected by `putnami.template.json` and a Go
project by `go.mod`. The task writes `metadata.json` as a whole document, never
by merging.

## Invariants

- A project without `putnami.template.json` is not packaged as a template.
- The version in the packaged manifest equals the version in the file name.
- The archive never contains the template project's own `putnami.json`.
- A dry run creates nothing.
- Staging happens in a temporary directory removed before the task returns, so
  it is never an output and the source tree is never mutated.

## Rejected alternatives

- **Copy the directory out of a checkout.** Only works with the checkout, and
  gives templates no version, upgrade path or pin.
- **Template packaging in each language extension.** Scaffolding Python would
  need the Go toolchain, and three packagers could disagree on the layout.
- **Render while packaging.** Rendering needs the target project's name and
  paths, which do not exist at package time, and would freeze the placeholder
  vocabulary into the archive format.
- **Ship `putnami.json` and let render overwrite it.** It declares
  `type: template` and `publish: template-archives`; the archive cannot enforce
  that render fixes it.

## Consequences

- The archive is not byte-reproducible: the host `tar` writes a timestamp in
  the gzip header. Identity comes from name and version, not the digest.
- The packager cannot validate content, so the committed templates are held to
  their contract by a conformance suite ([ADR 0002](0002-templates-are-checked-by-their-packager.md)).
- A new template needs only its manifest and project declaration.
