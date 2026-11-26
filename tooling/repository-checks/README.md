# @putnami/repository-checks

The checks that read this repository's files as text, on every change:

- the public-cut scan: no private repository, tracker reference, secret,
  private infrastructure or retired root in the candidate cut, against the
  reviewed baseline and allowlist in `tooling/cli/internal/cli/testdata`;
- the release rehearsal and its recorded verdict
  ([`tooling/cli/doc/20-release-rehearsal.md`](../cli/doc/20-release-rehearsal.md));
- the governance record answers the questions it claims to (`GOVERNANCE.md`,
  `RELEASING.md`, `SECURITY.md`, `CONTRIBUTING.md`, `README.md`,
  `protocols/support/README.md`);
- the README and the getting-started page print the block the first-use smoke
  runs;
- the URL the CLI cites for moved commands names a heading the site publishes;
- every pattern a `putnami.json` declares outside its project is spelled the
  way the cache key matches it;
- the shipped `go.mod` and `package.json` manifests name no Putnami Cloud
  package.

## Why it depends on nothing

The module imports nothing from this repository: no CLI package, no protocol,
no test helper. Its test task therefore depends on no other project's build or
tests, and a change anywhere plans this project's own tasks and nothing more.

A check against the CLI reads what the CLI commits instead of linking it: the
moved-commands check parses the declaration of
`CommandsThatLeftTheCoreURL` in
`tooling/cli/internal/commandmeta/moved_commands.go` as Go source, and the
CLI's own tests prove that its messages cite that constant. The spec proofs of
the release rehearsal are written in the observation format of the features
protocol's `spectest` package by a helper of this module.

The checks read the whole candidate cut, so the project declares
`options.test.filePatterns: ["git:**"]`: tracked files and non-ignored
additions, read through Git's own enumeration, with ignored local state left
out. Every change runs the suite; reading about 13 000 files costs less than
compiling anything to decide which of them a change could reach.

## What stays elsewhere

A check that needs the CLI's code stays in a project that imports it, with
inputs narrowed to the files it reads:

- [`@putnami/cli-documents`](../cli-documents/README.md) proves, with the
  CLI's cache-key collector, that this project's declaration keys exactly the
  files the public-cut scanner reads. It imports the scanner's enumeration from
  this module.
- [`@putnami/cli-document-set`](../cli-document-set/README.md) runs the CLI
  against the contributor recipe, the environments of `putnami.ci.json` and the
  cloudless fixture.

## Coverage

The package code is the public-cut scanner, which the tests here exercise over
the repository; coverage is off.
