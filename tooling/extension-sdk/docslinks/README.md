# `docslinks` — the documentation link check

This package decides what a broken link is in a project's documentation. Two tasks apply it:

| Task | Runs in | Checks | Why |
|------|---------|--------|-----|
| `lint-docs` of each language extension | `putnami lint` | the project's documents | feedback while you work on the project |
| `docs-links-validate` of the SDD extension | `putnami validate-workspace` | every document of the workspace, whatever the selection | the documents no project owns, projects without a language extension, and a link from one project into another that a change to the other breaks |

A link broken inside a selected project is reported by both tasks, and a project that lists two language extensions reports it once per extension. A language extension registers one task and supplies nothing else:

```go
"lint-docs": docslinks.Job(),
```

## What it checks

The documents are every `README.md` and every `.md` file under a directory named `doc`, as regular files or symbolic links to one. Inside a project, `doc` is looked for below the project root, so a project that sits under a `doc` directory is read like any other. The walk skips `node_modules`, `vendor`, `testdata` and `dist`, dot and underscore directories, and directories git ignores. A project's walk also stops at a nested directory that holds `putnami.json` or `go.mod`: that directory is another project, and its own lint checks it.

In each document, the check reads inline links and images, including a link whose text wraps a line, reference definitions, and the `href` and `src` attributes of `<a>`, `<img>` and `<source>`. It skips front matter, fenced and indented code (inside list items too), inline code, HTML comments and footnote definitions (`[^1]: ...`).

A link is broken when:

| The link | is broken when |
|----------|----------------|
| names a file or a directory | nothing exists at that path, or a path segment differs in case from the name on disk |
| climbs out of the workspace | always, and also when a symbolic link or a Windows directory junction on its path leads out: no reader of the repository can follow it, and the check does not read outside the workspace |
| holds a backslash that escapes nothing | always: the forge reads it as part of a name, and Windows as a separator |
| has an anchor, and its target is a `.md` file or the document itself | no heading of the target has that anchor, and no `id` or `name` attribute sets it |
| cannot be decoded | a `%` escape is invalid |

The check requires the case on disk because a link that resolves only on a case-insensitive disk is broken on Linux and on the forge. Links with a scheme (`https:`, `mailto:`), links that start with `//`, and absolute paths are not checked: an absolute path is a site route, which only the site that serves it can resolve. An anchor on any other file type, such as `main.go#L10`, is not checked. A backslash before an ASCII punctuation character is an escape, as in CommonMark: `real\_name.md` names `real_name.md`.

Anchors follow GitHub's rule. The heading renders first: links and images keep their text, HTML tags are dropped, entities such as `&amp;` become their character, and underscore emphasis is removed. The text is then lowercased, every character other than a letter, a mark, a number, `_` or `-` is dropped, and each space becomes `-`. A repeated heading gets `-1`, `-2` and so on. Code spans keep their text.

## The task

`Job()` fails the task with one `error` diagnostic per broken link, coded `docs-links` and located at the link's destination. It adds the count to the `lint-errors` metric. The `docs-links` lint flag turns the step off for one run (`--docs-links=false`) or for a project (`"options": { "lint": { "docs-links": false } }` in `putnami.json`). The project setting also removes the project's documents from `docs-links-validate`, which reads it as the CLI merges it: `*` (workspace only), `lint`, then the name of each of the project's own extensions and its `<extension>:lint` key, in `putnami.json` over `putnami.workspace.json`. As in the CLI, a block keyed by an extension's path is not read for this flag. A block keyed by `@putnami/go` reaches Go projects only; the job context names each project's extensions.

The task is uncacheable: a link may name any file of the workspace, so no file-pattern key covers what it reads, and a key that missed a deleted target would serve a green verdict for a broken link. Each run therefore starts the extension and one `git` process per project, even when every other lint task replays from the cache; in this repository one project's check takes about 0.3 s.

## Use it elsewhere

| Function | Use |
|----------|-----|
| `CheckProject(workspaceRoot, projectRoot)` | the findings of one project's documents |
| `WorkspaceDocuments(root, projects)` and `Check(workspaceRoot, files)` | every document of a workspace in one walk, grouped by the project that owns it and read as that project's lint reads it, and the findings of any set of documents |
| `Emit(emit, workspaceRoot, findings)` | report findings as diagnostics from another task |
| `Anchors(source)` and `Slug(text)` | the anchors a Markdown document defines |
