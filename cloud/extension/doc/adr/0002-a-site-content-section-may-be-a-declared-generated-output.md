# ADR 0002 — A site-content section may be a declared generated output

- **Status**: accepted
- **Scope**: `@putnami/cloud` (`cloud/extension`), the site-content `build`,
  `validate`, `package` and `publish` steps and the workspace probe

## Context

A site project (`publish: ["site-content"]`) publishes one bundle per
section, and until now every section was a committed directory. Some sections
are derived from other sources, such as a reference generated from code. A
committed copy of such a section drifts from its source unless someone
regenerates it by hand and commits the result.

The extension reads a site's sections in four places: validation, the
workspace probe that declares the release-set members, packaging and
publication. All four must agree on the sections and their files, or a
project that passes validation packages something else. The member's
selection fingerprint, the key that decides whether a publication
republishes the member, must move whenever the published bytes can change and
only then.

## Decision

**The extension runs a project-declared generator as a cached build step, at
the trust level of the project's own tests.** A project declares a generated
section in a `site-content.json` file at its root, under
`generated.<section>`, with a `command` argument list. The file holds that one
member and nothing else: an unknown member at any level is refused, so a
misspelled key fails instead of declaring nothing. The
`build~cloud-site-content-generate` step runs that command in the project directory with the job's environment, with
`{output}` replaced by the absolute path of `.gen/site-content/<section>`.
The command is project code, like a test: the extension adds no sandbox, no
toolchain and no network access of its own.

**The declared inputs are the key.** The step is cached and deterministic. Its
key holds `site-content.json`, where the commands live, `putnami.json`, and
the files the project lists in `options["@putnami/cloud:build"].filePatterns`,
which the engine adds to the key of every step of the extension's `build`. The generated files are
a function of that key.

**Readers wait for the step and read one location.** `validate` and `package`
plan `build` first for a site project, so the step that reads a section waits
for the step that writes it, and its key, and with it the member's selection
fingerprint, mixes the generate step's key. No reader's key reads
`.gen/site-content` itself: the generate step can rewrite it after the run
computes its keys, and the folded key already fixes those bytes. Validation,
packaging and publication read a declared generated section from
`.gen/site-content/<section>` and nowhere else. The probe lists the declared
names from `site-content.json` alone, so it stays a pure function of the
tree, and the file is one of its inputs, so a changed declaration re-probes.

**A dedicated file activates packaging.** The planner plans `package` for a
project only when it holds one of the command's activation files. A committed
section's landing page, `<section>/index.md`, is one; `site-content.json` is
another, as `image-layers.json` is for an image. A project whose only sections
are generated has no landing page to commit, and an extra committed section
would publish an extra member, so the declaration lives in its own file
rather than in `putnami.json`. Every project carries a `putnami.json`, so it
activates `build`, whose generate step does nothing without a declaration,
and not `package`. The generator's inputs stay in
`options["@putnami/cloud:build"].filePatterns` in `putnami.json`: that key
layer belongs to the engine, not to the declaration.

**Declaration decides, not presence.** A directory under `.gen/site-content`
that the project does not declare is no section. A declared section whose
directory is absent is refused, and so is a declared name that is also a
committed section directory.

## Consequences

- A generated section stays current without a committed copy, and its member
  republishes when a listed input or a command changes.
- A site project may consist of generated sections only.
- A declaration written in `putnami.json` declares nothing.
- A generator input the project does not list does not move the key: the
  section keeps its old content until a listed input changes. The project
  owns the completeness of the list, as it owns its test inputs.
- `putnami validate` on a site project runs `build` first, and with it the
  project's generators.
- Every site project's package step now waits for the generate step, so each
  site-content member's selection fingerprint moves once when this version of
  the extension is installed.
- A site project that is also a Go or TypeScript project cannot declare a
  generated section until those extensions cede `.gen/site-content`.
