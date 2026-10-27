# @putnami/windows-vet

The Windows compile gate (D-W11). `TestEveryGoWorkModuleVetsForWindows` runs
`GOOS=windows GOARCH=amd64 go vet work` from the repository root. The `work`
pattern matches every package of every `go.work` module, test files included, so
a Unix-only symbol anywhere in the workspace fails the contributor gate without a
Windows host. The test also fails when a `go.work` module has no package that
builds for Windows, since vet would then check nothing in it.

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task. The gate
used to live in `@putnami/cli-documents`, whose test key is the whole tracked
tree, so a change to a document re-ran a `go vet` of every module.

Here the key holds what `go vet` reads: the tracked `.go` and `.s` files, every
`go.mod` and `go.sum`, and `go.work` (`options.test.filePatterns`). A change that
touches none of them reuses the verdict. A file that a `//go:embed` directive
names is not an input: the build of the project that embeds it fails on the same
change.

The module requires no workspace module, so the project has no dependency edge:
it reads the other modules through the go command, not through imports.
