# ADR 0005 — Each project commits its own client inventories

- **Status**: accepted
- **Date**: 2026-09-30
- **Scope**: `@putnami/clientgen` (`tooling/clientgen-extension`), the
  `clientgen.external.json` and `clientgen.framework.json` inventories
- **Relates to**: `protocols/clientcontract` ADR 0011 (an external authority can
  own an operation), the `clientgen-external-v2` and `clientgen-framework-v2`
  schemas, and their version 1 predecessors

## Context

The guard reads two inventories. `clientgen.external.json` names the adapters
that speak a contract someone else owns. `clientgen.framework.json` names the
callsites no generated binding can replace. Each entry already names the
project that owns the adapter.

Both inventories were one file each at the workspace root. Two pull requests
that classified callsites in two unrelated projects edited the same file and
the same sorted array, so the second one to merge conflicted. The file mixed
entries of many owners, and nothing in its location said which project an
entry belonged to.

## Decision

**1. The inventory lives in the project directory.** A project's entries sit
in `<project>/clientgen.external.json` and `<project>/clientgen.framework.json`.
The guard reads the file of every project in the Putnami workspace project
index, in project order, and validates the merged entries with the rules it
already had. A project without the file declares nothing.

**2. A project's file names no project.** The directory already names it, so
repeating it in every entry is noise. A project file is `protocolVersion: 2`
(`clientgen-external-v2`, `clientgen-framework-v2`): the version 1 entry
without its `project` member, decoded as strictly as before, so a `project`
member fails as an unknown field. The machine report still carries each
entry's project, filled from the directory.

**3. Version 1 is the older root layout, never a project file.** A
`protocolVersion: 1` document, whose entries each name their project, is read
only as the older workspace-root file: the guard reads none of its entries and
reports one finding that names the project file each entry moves to. A version
1 document in a project directory fails. A workspace that still commits the
root files fails with the move to make, instead of losing the classifications
without notice.

**4. A version 2 root file belongs to a root project only.** When the index
holds a project at the workspace root (`.`), the root file is that project's
file. Otherwise a version 2 root file belongs to no project and fails.

## Consequences

- Two pull requests that classify callsites in different projects edit
  different files.
- An inventory change is a change to the project that owns it, so the project
  selection of a change covers it like any other project file.
- A workspace that commits root inventories moves each entry into its project's
  file once, dropping `project` and setting `protocolVersion` to 2. The
  root-file finding lists the destination files.
