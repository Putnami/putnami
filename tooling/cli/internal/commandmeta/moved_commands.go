package commandmeta

// CommandsThatLeftTheCoreURL is the published CLI reference section that lists
// every command that left the core CLI, the extension that serves it now, and
// its current spelling. A message about a moved or undeclared command cites
// this URL instead of naming an extension, so it stays true in any workspace
// and outside one. The page is tooling/doc/02-cli.md, published by
// sites/putnami.dev; tooling/cli-documents checks that the URL names one of its
// headings.
const CommandsThatLeftTheCoreURL = "https://putnami.dev/docs/tooling-%26-workspace/cli#commands-that-left-the-core-cli"
