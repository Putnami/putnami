# ADR 0004 — One packager for every content project

- **Status**: accepted
- **Scope**: `@putnami/scaffold` (`tooling/scaffold`), `@putnami/contributor`

## Context

A template and a content-only extension are the same kind of thing: a directory
of content no toolchain compiles, packaged into a versioned archive, uploaded by
the archive uploader, and materialized into another workspace. A content-only
extension declares `agentContent` and runs nothing; `@putnami/contributor` is
one. Both write `<command-output>/archives`, and every declared output has
exactly one owner.

## Decision

### 1. One task, one step per form

The `package` command activates on `putnami.template.json` and on
`putnami.extension.json`. It has two steps that run the one task
`package-content`, each with plan-time activation:

- `template` requires `putnami.template.json` and packages a template
  ([ADR 0001](0001-a-template-is-a-published-artifact.md));
- `agent-content` requires an `agentContent` key in `putnami.extension.json`
  and packages a content-only extension.

The task owns `archives/` and `metadata.json`. A project carrying both forms
fails; one carrying neither is skipped. A template whose extension manifest
declares no content is only a template. `src/skills/` alone selects nothing:
agent content ships inside an extension.

Two tasks would be the obvious shape and are rejected: both would claim
`archives/`, and mutual exclusion at run time is invisible to the manifest.

The step id `template` is a cross-extension contract. The Cloud extension's
release-set probe routes every `template-archives` member to this extension's
`template` step, and the release-set planner fails when no step carries that
id. A rename must land in that probe first and reach this workspace through its
pin.

### 2. The SDK owns the content builder

`agentartifact.PackageExtension` in the extension SDK builds and stamps a
content-only extension with the declared content policy
(`options.agent-artifact`). The CLI materializes a path-declared extension with
the same builder, so a path-declared and a registry-pinned workspace get the
same bytes. This extension owns packaging and publication only. The packaged
project supplies the name, the source layout and the policy; the packager holds
no artifact identity of its own.

An extension that declares agent content and also a runtime, commands, tools,
tasks, hooks or a workspace adapter fails this task: its language extension
packages it with its executable
([SDK ADR 0003](../../../extension-sdk/doc/adr/0003-every-packager-ships-agent-content-through-one-step.md)).

### 3. One archive, written under every platform key

The CLI's extension installer asks the registry for the host's own os/arch. A
content-only extension's archive is platform independent, so the task writes
the same bytes as `<encoded-name>-<os>-<arch>.tar.gz` for every key of
`pkgmeta.ArchivePlatforms`, the SDK matrix the Go packager also reads, on the
`archives` channel. The publication manifest is not a template: the uploader
keys each file by the platform its name carries.

## Invariants

- A project activates at most one packager; two forms fail.
- Two packaging runs over the same content-only extension source and version
  produce identical bytes. A template archive is not held to this.
- A source file outside the declared layout, or an emitted file that violates
  the content policy, fails the job.

## Rejected alternatives

- **One `linux-x64` archive.** The extension installer asks for the host's key,
  so every other host would fail to resolve the extension.
- **One archive with the template fan-out marker.** Couples this package to how
  the out-of-tree uploader treats templates.
- **A builder per content project.** A second copy of the emission rules lets
  path-declared and registry-pinned workspaces drift apart.

## Consequences

- A new content-only extension is a directory, a manifest and a `putnami.json`
  naming `@putnami/scaffold`.
- A platform added to `pkgmeta.ArchivePlatforms` reaches both packagers at once.
- Checks on a shipped extension's content (member list, frozen policy) live
  with the project that ships it; the builder's fixture tests live in the SDK.
