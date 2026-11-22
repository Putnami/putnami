# TypeScript documentation examples

`putnami test` here type-checks every TypeScript code block (`ts`,
`typescript`, `tsx`) of the markdown documents `examples.json` names, with the
TypeScript compiler, against the packages of this workspace. An example that
no longer compiles fails the test.

- Each block is checked as a module. A block that only holds class members,
  such as a repository method, is checked inside a class.
- A block may use names it does not declare. When another block of the same
  document imports a name the block uses, the check imports it the same way.
  Otherwise a name exactly one framework package exports is imported from it
  when the block calls it, renders it or reads an existing property from it.
- Unused names, possibly undefined values, signature listings and a name
  declared again to show an alternative are tolerated. Every other error
  fails.
- Mark a fence `ts nocheck` for pseudo-code or a block with elisions.

To check a new document, add its path or a glob that matches it to
`examples.json`, and a test `filePatterns` entry of `putnami.json` that covers
it: the declared inputs decide when the check runs again, and the test fails
when a checked document is not one of them.
