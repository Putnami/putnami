# ADR 0086 — `init` declares the languages a repository holds

- **Status**: accepted
- **Date**: 2026-11-10
- **Decision**: D-067 of the root `decisions.json`; D-008 stays as written
- **Scope**: `@putnami/cli` (`internal/commands/lifecycle`: `workspace_init.go`,
  `init_languages.go`, `init_languages.json`; `internal/workspace`:
  `project_scan.go`; `internal/commandmeta`)

## Context

D-008 settles that `putnami init --extension <language>` installs that
language extension, and that its dependencies bring the rest. `init` accepted
three languages, `ts`, `go` and `py`, from a table in the CLI that paired each
with a starter template. The seven stack extensions (`@putnami/rust`, `java`,
`swift`, `dart`, `dotnet`, `ruby`, `php`) ship no template, and a team adopted
one by hand: declare it in `putnami.workspace.json`, then run
`putnami projects sync`. A table in the core meant a core change for every new
stack, the failure the workspace adapter (`protocols/extension/workspace.go`)
removed from project discovery: the core scan (`internal/workspace/project_scan.go`)
knows one marker of its own, `putnami.json`.

Every language extension already owns what `init` needs. Its manifest's
`workspace` adapter names its language (`workspace.language`), the files that
mark one of its projects (`markers`) and the directories its scan skips
(`excludes`).

`init` cannot read that manifest before it installs the extension. The
manifest is inside the extension's archive. The Put registry answers one
download per artifact on a channel, and has no listing and no read of the
manifest alone. A release set lists every artifact of a publication, but it is
resolved through a provider extension that a directory without a workspace
does not have, and it needs the publisher's namespace, which a consumer does
not know ([ADR 0020](0020-upgrade-resolves-channels-natively.md),
[ADR 0056](0056-init-resolves-on-one-channel.md)). Downloading every language
extension to read ten manifests would cost several archives per `init`, and
fail offline, before the user has chosen anything.

## Decision

### 1. The languages come from the extensions' adapters, through a generated index

The CLI embeds `init_languages.json`: one entry per extension whose manifest
declares `workspace.language`, with its name, its language, its markers and
its excludes, copied from that manifest. Nobody edits the file.
`TestInitLanguagesFollowTheExtensionManifests` rebuilds it from the manifest of
every extension the root `putnami.workspace.json` declares by path and fails
when the committed file differs; `PUTNAMI_UPDATE_INIT_LANGUAGES=1` rewrites
it. Every extension whose source lives in this repository is declared there,
so a stack joins `init` by declaring `workspace.language` in its own manifest,
with no Go change.

This is the smallest data-driven copy that `init` can read before an install.
Its cost is the tradeoff below.

### 2. `--extension` takes one language or a list

`--extension` takes a language of the index, or a comma-separated list of them
for a polyglot repository (`--extension rust,java`). `ts` and `py`, the
shorthands `init` accepted before, still name `typescript` and `python`; a new
language takes no shorthand. A language named twice is declared once. An
unknown language, an empty item and an empty value are usage errors that list
the languages, raised before anything is written. Without `--extension`,
`init` declares `typescript`, unless point 5 applies.

### 3. A starter is `init`'s choice, and `--project` needs one

A starter is the template `--project` renders and the registries its
dependencies need: `typescript-web` with the `@putnami` npm scope,
`go-server` and `python-server`. Which template greets a new user is a product
choice of `init`, not language knowledge, so the starters stay in the CLI,
keyed by language. The drift test checks that each starter's template exists
next to its language extension and that the template manifest names that
extension.

`--project` renders the starter of the first language of `--extension`. A
language without one is a usage error raised before anything is written, and
it names the languages that have one. Every declared language that has a
starter declares that starter's registries, as `--extension ts` did.

### 4. `init` adopts what the repository holds

After it installs the declared extensions, and after the starter project when
`--project` is given, `init` looks for the markers of the declared languages,
by the walk of point 7. When it finds one, it runs `putnami projects sync`,
then installs the extensions the sync declared, as `init --from` does
([D-064](../../../../decisions/D-064.json)). A language whose markers are absent is
declared and installed, and nothing is synchronized.

### 5. Without `--extension`, `init` proposes what it detects

`putnami init` with no `--extension`, no `--from` and no `--project` looks for
the markers of every language of the index, by the walk of point 7:

- **In a terminal**, it lists each detected language with the first directory
  that holds one of its markers and asks which to declare, all of them
  preselected: an empty answer declares them all. It then runs as
  `--extension <answer>`, so point 4 adopts them.
- **Without a terminal**, it declares nothing and writes nothing. It fails with
  a usage error that names the detected languages, and suggests
  `putnami init --extension <detected>`.
- **With no marker**, it keeps the TypeScript default.

The no-terminal rule is the safer of the two choices offered. Keeping the
previous behavior would declare TypeScript in a repository of another
language and run its workspace installer there. Declaring the detected
languages unasked would run their sync tasks, which rewrite tracked manifests
(the Go sync tidies `go.mod` files, the Python sync renames `pyproject.toml`
projects), in a run nobody watches. A failure writes nothing, its exit status
stops a script before it builds on a workspace that does not exist, and the
suggested command is the one-line fix. A script that names `--extension`
behaves the same with or without a terminal.

`--project` without `--extension` keeps the TypeScript starter, as before: a
project needs a starter, and the user asked for one.

### 6. A monorepo tool marker points at `--from`

Before it looks for languages, `init` checks the root for the markers of the
tools `--from` accepts (`nx.json`, `turbo.json`, `rush.json`,
`.moon/workspace.yml`, `pants.toml`, `MODULE.bazel`, `WORKSPACE`,
`WORKSPACE.bazel`). When one is present, it declares nothing, writes nothing,
and fails with a usage error that names each tool it found and suggests
`putnami init --from <tool>` for the first one. The tool's extension decides
which packages are projects and which language extension each one gets; a
language guess beside it would contradict that, so `init` acts on nothing.

### 7. Detection reads names, never contents

The walk starts at the directory `init` runs in, the root included. It skips
`.git`, `.putnami`, the directories git ignores and every directory that any
language of the walk excludes, the rules the project scan applies. It matches
each directory against the markers as file patterns, reads no file, and stops
once every language has a match. It costs one scan of the tree that
`projects sync` makes anyway, and adds no dependency on the
`putnami observe` detectors.

## Consequences

- `putnami init --extension rust` in a Rust repository declares and installs
  `@putnami/rust` and synchronizes its crates: the hand adoption of each stack
  document becomes one command.
- **A new stack reaches `init` with the CLI release that carries its index
  entry.** The drift test makes that entry part of the change that adds the
  stack, and one tagged publish releases every version line, so a stack
  published from this repository is accepted by the CLI of its own release.
  No Go code changes. An extension published from another repository is not
  in the index: it is installed with `putnami extensions install <name>` and
  adopted with `putnami projects sync`.
- `--extension` is no longer a closed enum in the command catalog: a list
  cannot be one. `init` validates it.
- A script that ran `putnami init` with no flag in a directory that holds a
  language or monorepo tool marker now fails with the command to run, instead
  of declaring TypeScript.
- `putnami init --extension ts` in a directory that holds a `package.json`
  now runs `projects sync`, which registers its packages.

## Rejected alternatives

- **Downloading each language extension to read its manifest.** It costs one
  archive per language on every `init` in a non-empty directory, needs the
  network before the user chose anything, and still needs a list of names to
  download.
- **A registry index of extensions.** The Put registry has no listing, and
  the release set needs a provider and a namespace `init` does not have.
- **A starter field in the extension manifest.** Manifests decode strictly,
  so a new member needs a contract bump that every older CLI refuses, for data
  only `init` reads.
- **A starter flag in the template manifest.** Template manifests decode
  strictly too, and every CLI reads templates at `projects create`.
- **Declaring the detected languages without a terminal.** See point 5.
- **Detecting through the `putnami observe` detectors.** They read build
  files and answer far more than a marker; the core does not depend on them.
