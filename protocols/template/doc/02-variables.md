# Template Variables

Template files (`.template` extension) are rendered with variable substitution
before the suffix is stripped. Files without the suffix are copied verbatim.

This page documents **engine behaviour**, not wire contract: the syntax and the
variable set are owned by `@putnami/cli` and are deliberately not declared in
`putnami.template.json` (see
[ADR 0001](adr/0001-manifest-declares-identity-only.md)).

## Syntax

Substitution uses ERB-style delimiters, with exactly one space on each side of
the name:

```
<%= projectName %>
```

The match is literal. `<%=projectName%>`, `<%= projectname %>` and
`{{projectName}}` are not substituted and are copied through unchanged.

A variable whose value is empty is **not** substituted: the placeholder is left
in place rather than replaced with an empty string, so a missing value is
visible in the rendered output instead of silently vanishing.

## Variables

The engine provides six variables:

| Variable | Meaning | Value under `dev template test` |
|----------|---------|---------------------------------|
| `<%= projectName %>` | The new project's name | `test-project` |
| `<%= projectPath %>` | The project's workspace-relative path | `test-project` |
| `<%= projectModule %>` | A language-safe identifier derived from the name (lower-cased, non-alphanumerics folded to `_`) | `test_project` |
| `<%= putnamiVersion %>` | The Putnami version to depend on | `latest` |
| `<%= workspaceRelativePath %>` | The path from the project back to the workspace root | `..` |
| `<%= goFrameworkVersion %>` | The Go framework version to depend on | `v0.0.0` |

## Directory names

Path components are substituted too, but only for one magic name: a directory or
file named `__module__` is replaced with the value of `projectModule`. This lets
a Go template ship `internal/__module__/` and have it land as
`internal/my_app/`.

No other placeholder is expanded in paths.

## Example

A `package.json.template` file:

```json
{
  "name": "<%= projectName %>",
  "version": "0.1.0",
  "scripts": {
    "build": "putnami build ."
  }
}
```

Rendered for a project named `my-app`:

```json
{
  "name": "my-app",
  "version": "0.1.0",
  "scripts": {
    "build": "putnami build ."
  }
}
```

## Overriding values when testing

`dev template test` renders with the defaults in the table above. A template
whose scaffolding cannot build under those defaults overrides them through
`testVariables` in its manifest. Only two keys are honoured — `projectName` and
`projectModule`:

```json
{
  "name": "my-template",
  "description": "…",
  "testVariables": {
    "projectName": "my-custom-test-name",
    "projectModule": "my_custom_test_name"
  }
}
```

`testVariables` can only override a variable the engine already provides; it
cannot introduce a new one.
