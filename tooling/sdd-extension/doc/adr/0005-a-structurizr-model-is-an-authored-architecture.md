# ADR 0005 — A Structurizr model is an authored architecture

- **Status**: accepted
- **Settled**: 2026-11-07, decisions D-061 and D-062 of the root
  `decisions.json`, approved by the owner with the plan of the Structurizr import
- **Amends**: nothing. `architecture sync` still never writes an import, and
  ADR 0001 of `protocols/architecture` still forbids turning an observation
  into a permission.
- **Scope**: `@putnami/sdd` (`tooling/sdd-extension`), the
  `putnami architecture import` command

## Context

Some teams already describe their architecture in Structurizr, as a DSL file
or a JSON workspace. The model names software systems, containers and
components, and the relationships between them. A team like that should not
have to write `putnami.architecture.json` files from scratch.

The authoring commands have a firm rule: a generator writes only the
mechanical half of a manifest. `architecture init` writes no export, no import
and no binding. `architecture sync` writes bindings only for an import a human
already declared. The reason is
[ADR 0001](../../../../protocols/architecture/doc/adr/0001-declarations-are-authority-observations-are-evidence.md):
an observed dependency is evidence, never a permission.

A Structurizr relationship is not an observation. A person wrote it into a
model and reviewed it there. Copying it into a manifest moves one declaration
from one format to another. It does not turn code into a permission.

## Decision

1. **`putnami architecture import --from structurizr <file>` writes domains,
   exports and imports from the model.** (D-061)
   - Each container is a domain whose ID is `<system>/<container>`. A software
     system with no containers is a domain whose ID is `<system>`. A
     `putnami.domain` property overrides the ID. On a software system, the
     property also applies to every container that does not set its own, so
     one property puts a whole system in one domain.
   - A container, a component or a software system with no containers whose
     `path` property names a project maps that project to its domain.
     `--path-property` names another property. The issue that asked for the
     command also offered a tag. A tag carries no value, so it cannot name a
     path, and the command reads only a property.
   - A relationship between two elements in different domains becomes one
     import of the consumer domain per producer domain, in mode `reference`.
     Of the five access modes, `reference` is the only one that claims no data
     crosses the boundary. The import reads the producer's generated
     `<domain>.interface.v1` export, or the one active export of the producer
     that allows reference access, and takes that export's facts. A producer
     with no export gets the generated one. A producer with several such
     exports, or none, leaves the import to a person, and the relationship is
     listed. The relationship's description and technology become the
     justification. A person refines the mode and the facts by hand.
   - Bindings follow the `architecture sync` rule. The command binds a project
     edge only when the graph observes it, and only to the one import that
     covers that domain pair. An edge between two domains that the model does
     not relate stays an `architecture validate` violation, and the command
     lists it.
   - An element the command cannot map is listed with the reason: a person, a
     component with no path, a path that is no project, or a relationship that
     stays inside one domain.
2. **A rerun only adds.** (D-062) Without `--apply`, the command writes
   nothing and prints the diff. With `--apply`, it creates the missing
   manifests and adds the missing projects, exports, imports and bindings.
   It never changes or removes an entry that already exists, so a hand edit
   survives every rerun. A domain pair that already has an import gets no
   second one, even when a person renamed the generated import. A new import
   takes the version of the export it reads. A changed manifest keeps the
   order of its entries, and the additions are appended. It is written in the
   layout of the protocol's canonical writer, so a manifest any Putnami writer
   produced keeps every existing line, and a manifest in another layout is
   rewritten in that layout once. It reports an import the manifest holds and the model
   no longer has, and leaves the removal to a person. A new manifest goes to
   `architecture/<domain>/` below the workspace root, or below `--at`. A
   domain that is already declared keeps its own file. The command refuses,
   before it writes anything, a new manifest whose path is taken or lies in a
   directory architecture discovery skips.
3. **The reader uses the standard library only.** It reads a JSON workspace
   and the DSL statements that describe a model: elements, groups,
   relationships, properties and tags. It skips views, styles, deployment
   environments, documentation and decision directives. Any other statement,
   `!include` among them, stops the command at its line, with the advice to
   export the model as JSON with the Structurizr CLI.

## Consequences

- A Structurizr team gets manifests that `putnami validate` accepts. A code
  dependency the model does not allow fails the gate.
- A rerun after a model change is a reviewable diff, and the git diff of an
  applied run is the authorization moment, as for `sync`.
- Generated exports and imports are deliberately thin: one `reference` export
  and one fact per producer domain. A team that needs richer contracts edits
  the manifest, and later imports keep those edits.
- Removal is manual. A relationship deleted from the model leaves its import
  in place until a person removes it. The command reports it so it does not go
  unnoticed.
