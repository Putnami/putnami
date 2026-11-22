// Package docexamples type-checks the Go code blocks of markdown documents
// against the packages this workspace resolves through go.work, so that an
// example that no longer compiles fails the gate.
//
// A block is checked unless its fence carries the nocheck word ("```go
// nocheck"). A block may be a complete file, top-level declarations,
// statements, or declarations followed by statements. A package the block
// qualifies without importing it is imported for it: the path another block
// of its document imports under that name, else the one standard or
// workspace package of that name that exports a selector the block reads,
// else the framework module of that name when it is the only such package,
// the selectors are not members of its types, and no third-party dependency
// of that name exports one of them. A type another
// block of the document declares is carried over the same way.
// Other names the block does not declare are tolerated; everything else must
// type-check.
package docexamples
