# @putnami/scaffold

Packages a project template into a distributable archive, so `putnami projects
create` can scaffold from a published artifact rather than from a checkout of
this repository.

## What it does

The extension activates on `putnami.template.json` and on a
`putnami.extension.json` that declares `agentContent`, and contributes one
command:

| Command | Task | Produces |
|---------|------|----------|
| `putnami package` | `package-content` | `<command-output>/archives/<name>-<version>.tar.gz` for a template, `<command-output>/archives/<encoded-name>-<os>-<arch>.tar.gz` for each registry platform key of a content-only extension (identical bytes), plus the matching `template-archives` or `archives` entry in the project's package metadata index |

A project carries one content form; two fail the job. Agent content ships only
inside an extension: a bare `src/skills/` tree selects nothing
([ADR 0004](doc/adr/0004-one-packager-for-every-content-project.md)). A content-only extension
declares agent content and runs nothing: the SDK's `PackageExtension` builds
its content with the declared policy, stamps the packaged form and
`cliContract` 5, and refuses an extension that also has a runtime, commands,
tools, tasks or hooks, which its language extension packages instead
([ADR 0004](doc/adr/0004-one-packager-for-every-content-project.md)).
`@putnami/contributor` is packaged this way.

```bash
# Build the archive.
putnami package --projects typescript-web

# Report the archive it would build, without creating anything.
putnami package --projects typescript-web --dry-run

# Strip the pre-release suffix from the packaged version.
putnami package --projects typescript-web --stable
```

The version comes from the invocation's resolved version when there is one, and
from the workspace version otherwise. The archive's manifest is stamped with the
same version its filename claims; a stamp that cannot be written fails the job
rather than shipping a version-less manifest.

## What the archive contains

Everything in the template directory **except** the entries that describe the
template as a project in *this* workspace:

| Excluded | Why |
|----------|-----|
| `putnami.json` | Declares `"type": "template"` and the `template-archives` publish channel — both meaningless in a scaffolded project |
| `putnami.features.json` | States the intent of the template, not of the project it renders into |
| `specs/` | Links decision records that exist only in this repository |
| `node_modules/` | An installed dependency tree, never template content |
| Anything starting with `.` | Local state and VCS metadata |

Rendering — which files carry placeholders, what the placeholders mean, how a
template is discovered and installed — is `@putnami/cli` behaviour, deliberately
outside the [template manifest protocol](../../protocols/template/README.md).
This extension only stages, stamps and archives.

## The committed templates

This extension also holds the workspace's committed templates to their contract.
`internal/packaging/committed_templates_test.go` discovers every
`putnami.template.json` in the tree and checks that its manifest validates, that
its project declaration agrees with it, that placeholders appear only in
`.template` files, that every JSON template renders to valid JSON, that an
application template is servable, that a Go template requires what it imports,
and that the real packaging job over the real template produces a consumable
archive.

Template projects carry no `go.mod` and no `package.json`, so no language
toolchain sees them: this suite is the only guard between a broken committed
template and a user's empty workspace. The template directories are therefore
declared as dependencies of this project, which puts this suite in the
`--impacted` plan for a change that touches only a template.

Go's own test cache keys on this package's inputs, so an ad-hoc run after
editing only a template can replay a cached pass. Re-run it with
`--enforce-coverage`, which forces `-count=1` — the verification gate always
does:

```bash
putnami test --projects @putnami/scaffold --enforce-coverage
```

## Support and contract

`@putnami/scaffold` is a public, documented, maintained package classified
`stable` in the workspace [support catalog](../../putnami.support.json). The
[project-scaffolding specification](specs/project-scaffolding.json) states the
observable promise, and five accepted decisions explain the durable choices
behind it:

- [a template is a published artifact, not a directory to copy](doc/adr/0001-a-template-is-a-published-artifact.md);
- [the committed templates are checked by their packager](doc/adr/0002-templates-are-checked-by-their-packager.md);
- [what a default template promises](doc/adr/0003-what-a-default-template-promises.md);
- [one packager for every content project](doc/adr/0004-one-packager-for-every-content-project.md);
- [templates run against the workspace framework](doc/adr/0006-templates-run-against-the-workspace-framework.md).

The archive is **not** byte-reproducible: it is produced by the host `tar`,
whose gzip header carries a timestamp, so a template's identity is its name and
version rather than an archive digest. Uploading belongs to `publish`. Before
v1.0.0, minor `0.x` releases may contain documented breaking changes — see
[RELEASE.md](../../RELEASE.md).

## License

[FSL-1.1-MIT](../../LICENSE.md)
