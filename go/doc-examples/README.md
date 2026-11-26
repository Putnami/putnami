# Go documentation examples

`go test` here type-checks every Go code block of the markdown documents
`examples.json` names against the packages this workspace resolves through
`go.work`. An example that no longer compiles fails `putnami test`.

- A block may be a complete file, top-level declarations, statements, or
  declarations followed by statements.
- A block may use names it does not declare. When another block of the same
  document imports the package a name qualifies, or declares the type a name
  refers to, the check carries that import or declaration over. Otherwise a
  qualifier is imported from the one standard or workspace package of that
  name that exports a selector the block reads. A framework module that is
  the only such package of its name is imported even when none of the
  selectors exist, unless they are members of its types or of the document's
  workspace imports, or a third-party dependency of that name exports one of
  them: `inject.New` fails, while `client.Do` on a client variable and
  `grpc.NewServer` from google.golang.org/grpc do not.
- Unused names, a return from a block of statements, and a name declared again
  to show an alternative are tolerated. Every other type error fails.
- Mark a fence `go nocheck` for pseudo-code or a block with elisions.

To check a new document, add its path or a glob that matches it to
`examples.json`, and a test `filePatterns` entry of `putnami.json` that covers
it: the declared inputs decide when the check runs again. The test fails when
a checked document, a workspace package the examples import, or a file such a
package embeds, is not one of them.
