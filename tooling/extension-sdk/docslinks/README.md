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

Inside a Git work tree, the check reads the repository's candidate cut and nothing else: the tracked files and the untracked files no ignore rule excludes, as `git ls-files --cached --others --exclude-standard` lists them. A file Git ignores, an empty directory, and a symbolic link to anything outside the cut do not exist for it, as they do not exist in a clone. Outside a Git work tree, the check reads the disk.

In each document, the check reads inline links and images, including a link whose text wraps a line, reference definitions, and the `href` and `src` attributes of `<a>`, `<img>` and `<source>`. It skips front matter, fenced and indented code (inside list items too), inline code, HTML comments and footnote definitions (`[^1]: ...`).

A link is broken when:

| The link | is broken when |
|----------|----------------|
| names a file or a directory | nothing exists at that path, or a path segment differs in case from the name on disk. Inside a Git work tree, a path exists when it is in the candidate cut: a link to an ignored file is broken |
| climbs out of the workspace | always, and also when a symbolic link or a Windows directory junction on its path leads out: no reader of the repository can follow it, and the check does not read outside the workspace |
| holds a backslash that escapes nothing | always: the forge reads it as part of a name, and Windows as a separator |
| has an anchor, and its target is a `.md` file or the document itself | no heading of the target has that anchor, and no `id` or `name` attribute sets it |
| cannot be decoded | a `%` escape is invalid |

The check requires the case on disk because a link that resolves only on a case-insensitive disk is broken on Linux and on the forge. Links with a scheme (`https:`, `mailto:`), links that start with `//`, and absolute paths are not checked: an absolute path is a site route, which only the site that serves it can resolve. An anchor on any other file type, such as `main.go#L10`, is not checked. A backslash before an ASCII punctuation character is an escape, as in CommonMark: `real\_name.md` names `real_name.md`.

Anchors follow GitHub's rule. The heading renders first: links and images keep their text, HTML tags are dropped, entities such as `&amp;` become their character, and underscore emphasis is removed. The text is then lowercased, every character other than a letter, a mark, a number, `_` or `-` is dropped, and each space becomes `-`. A repeated heading gets `-1`, `-2` and so on. Code spans keep their text.

## The task

`Job()` fails the task with one `error` diagnostic per broken link, coded `docs-links` and located at the link's destination. It adds the count to the `lint-errors` metric. The `docs-links` lint flag turns the step off for one run (`--docs-links=false`) or for a project (`"options": { "lint": { "docs-links": false } }` in `putnami.json`). The project setting also removes the project's documents from `docs-links-validate`, which reads it as the CLI merges it: `*` (workspace only), `lint`, then the name of each of the project's own extensions and its `<extension>:lint` key, in `putnami.json` over `putnami.workspace.json`. As in the CLI, a block keyed by an extension's path is not read for this flag. A block keyed by `@putnami/go` reaches Go projects only; the job context names each project's extensions.

The task is cached. A link may name any file of the workspace, so the key is the workspace input `git:**`, which holds the candidate cut the check reads: each candidate's path and bytes, and a symbolic link's target text. Deleting or renaming a link target, or editing a heading an anchor names, moves the key. Adding an ignored file, or staging a change, does not. The project input `README.md` names the document the check starts from, so the project side of the key does not fold the project's ignored build output. `Inputs()` returns these ports, and each language extension's manifest test holds its `lint-docs` declaration to them.

Because the key holds the whole cut, any change to a candidate file reruns the check in every project, and `--impacted` selects every project's `lint-docs` for it. A run on an unchanged tree replays, whatever the commit, the branch or the checkout. Outside a Git work tree the `git:` input has no key, so the task runs every time.

## Use it elsewhere

| Function | Use |
|----------|-----|
| `CheckProject(workspaceRoot, projectRoot)` | the findings of one project's documents |
| `NewReader(root)` | one reader of the candidate cut, or of the disk outside a Git work tree, whose `ProjectDocuments`, `WorkspaceDocuments`, `Check` and `ReadFile` all read the same files: a task that reads more of the workspace than the documents reads it here, so its `git:**` key holds all of it |
| `Inputs()` | the input ports a `lint-docs` task declares |
| `WorkspaceDocuments(root, projects)` and `Check(workspaceRoot, files)` | every document of a workspace in one walk, grouped by the project that owns it and read as that project's lint reads it, and the findings of any set of documents |
| `Emit(emit, workspaceRoot, findings)` | report findings as diagnostics from another task |
| `Anchors(source)` and `Slug(text)` | the anchors a Markdown document defines |
