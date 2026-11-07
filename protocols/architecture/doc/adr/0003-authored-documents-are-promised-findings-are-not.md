# ADR 0003: Authored documents are promised, findings are not

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/architecture`, `@putnami/sdd`

## Context

The module was `experimental`: free to change its wire format, finding IDs and
detector coverage without a migration. A team that declares its architecture to
protect its repository from regressions cannot accept that for the files it
writes. Putnami Cloud, a separate repository, already parses team manifests
with the published module.

The module carries two kinds of documents:

- **Authored** documents a team writes and commits: the domain manifest
  (`putnami.architecture.json`), the adoption baseline
  (`architecture.baseline.json`) and the waivers
  (`architecture.waivers.json`).
- **Derived** output the tooling computes: finding IDs, diagnostic codes, the
  snapshot, and detector coverage. These still move as detection improves.

A baseline or waiver entry names a finding ID. The authored file stays valid
when an ID changes, but its entry goes stale and the gate fails.

## Decision

1. **The module is `preview`.** The workspace-root
   [`putnami.support.json`](../../../../putnami.support.json) records it, with
   no `default` claim. It is not `stable`, because its finding IDs and detector
   coverage still change.
2. **Version 1 of the authored documents is fixed.** A manifest, baseline or
   waivers file that is valid under `protocolVersion: 1` stays valid. A change
   that would reject one opens version 2, which ships with a
   `putnami architecture migrate` command that rewrites version 1 files.
3. **Derived output is outside the promise.** Finding IDs, diagnostic codes,
   the snapshot schema and detector coverage may change in any release. The
   release notes name each change.
4. **A frozen corpus holds the promise.** `fixtures/v1/` holds a manifest set,
   a baseline and a waivers file as written on 2026-11-07. A test parses and
   validates every file and pins every file's SHA-256.
5. **The gate says when it depends on finding IDs.** `validate-workspace` warns
   with `architecture.finding_ids_not_promised` when the workspace commits a
   baseline or a waivers file. The warning names the file and says that an
   upgrade can rename the IDs it lists. It never fails the gate.
6. **A version stays supported for a defined time.** Until 1.0, a minor release
   may end a version only with a migration documented in the release notes.
   From 1.0, a version stays readable for 12 months after its successor ships.

## Rejected alternatives

- **Keep `experimental` and add a `preview` feature entry for the manifest.**
  Support ADR 0002 §5 uses a feature entry to lower a status inside a subject,
  not to raise one, and `experimental` means no promise at all.
- **Promise finding IDs too.** Detection is still growing. Freezing IDs now
  would block every detector improvement behind a version bump.
- **Warn on every gated workspace.** Every adopter would see the warning,
  including the ones that commit no file naming a finding ID. Those workspaces
  cannot be broken by a renamed ID.

## Consequences

- A team can write manifests, a baseline and waivers and keep them valid
  across releases, or get a migration command.
- A workspace with a baseline or waivers reads a warning on each
  `validate-workspace` run until it removes the file.
- Promoting to `stable` needs frozen finding IDs and detector coverage, and a
  `reviewedStableSubjects` entry.
