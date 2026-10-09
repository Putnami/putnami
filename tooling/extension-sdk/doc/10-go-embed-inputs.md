# Go embed source inputs

`goembed` is the shared reader for the CLI's `go-embed:build` and
`go-embed:test` selectors and the Go module packager. `Patterns` parses actual
`//go:embed` comments. `Resolve` returns their regular payload files;
`ResolveInputs` adds lexical `.go` sources and the regular referents of
supported source-file symlinks. The CLI hashes these inputs as raw bytes.
`SelectsPath` recognizes selected lexical sources, payloads and referents,
including deleted paths, so source impact can select their owning task or fail
before cache reuse. `ReadSource` is the packager's source reader;
`SourceReferent` gives the CLI the same validated target for an already
selected `.go` alias in a directory the Go source scanner ignores.

A supported source link is direct and relative, points to a regular file in
the same project and Go module, and has no symlink component in its target
path. Its directives resolve relative to the lexical `.go` package directory.
Absolute, broken, escaping, chained, directory and irregular links fail.
Payload links fail independently; directory source links are not traversed.

`BuildConstraintExpr` reads the actual leading build-comment header. A
`//go:build` line inside a block comment has no effect, and legacy `// +build`
lines require the blank separator before the package clause. The first valid
`//go:build` expression takes precedence over legacy lines.
`ConstraintBuildable` evaluates whether any ordinary tag assignment can
satisfy the expression, with `ignore` false. `ToolingIgnoresPath` supplies the
shared Go source exclusion for `testdata` and dot/underscore-prefixed path
segments. The resolver also excludes `vendor` and nested modules. Plain
`out`, `dist` and `node_modules` segments do not exclude a Go subpackage.

The local reader can select an ignored payload or source referent in `.gen`,
`node_modules`, `.putnami` or a declared output. For a portable CLI request,
admission refuses such a semantic input when native source capture would omit
it, naming the task and path. Tracked or non-ignored inputs at the same paths
are capturable. Other generated inputs keep their existing lifecycle policy.
