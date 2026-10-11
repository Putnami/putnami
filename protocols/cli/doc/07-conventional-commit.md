# Conventional commit reading

A release reads commit messages twice. The version bump reads them to choose
the next version. The API checks read them to find the marker that allows an
incompatible change. Both must give one message the same reading: a marker one
accepted and the other ignored would ship a breaking change in a release that
promises none. `ParseConventionalCommit` is that one reading.

It takes the subject (the first line of the message) and the body (the rest),
and returns the type, the scope and whether the commit declares a breaking
change:

```text
feat(cli)!: drop the --all flag
```

```text
refactor: drop the greeting

BREAKING CHANGE: Greet is gone; use Wave.
```

| Message | Type | Scope | Breaking |
| --- | --- | --- | --- |
| `feat: add` | `feat` | | no |
| `fix(cli): stop` | `fix` | `cli` | no |
| `feat(cli)!: change` | `feat` | `cli` | yes |
| `refactor: move`, with a `BREAKING CHANGE:` body line | `refactor` | | yes |

## The subject

The subject is a conventional commit when the text before its first colon is a
type, an optional scope in parentheses, and an optional `!`:

- The type is one or more lowercase ASCII letters. The function accepts any
  such word: which types advance a version is the caller's rule.
- The scope is the text between the first `(` and a closing `)` that ends the
  scope. Nothing is checked inside it.
- `!` directly before the colon declares a breaking change.

Any other subject is not a conventional commit, and the function reports it
with `ok` false. That covers a subject with no colon, a merge commit
(`Merge pull request #1 from a/b`), the subject Git writes for a revert
(`Revert "feat!: drop the flag"`), `fixup! feat: add`, and `WIP: add`. Such a
message declares nothing, whatever its body says.

## The breaking footer

A body line that starts with `BREAKING CHANGE:` or its synonym
`BREAKING-CHANGE:` declares a breaking change. Spaces before the token are
ignored. The token is uppercase: `breaking change:` declares nothing, and
neither does the token in the middle of a line.

## Differences from Conventional Commits 1.0.0

The function follows the
[specification](https://www.conventionalcommits.org/en/v1.0.0/) except here:

| Input | Specification | This function |
| --- | --- | --- |
| `Feat: add`, `FIX: stop` | A type in any case is valid | Not a conventional commit |
| `feat:add`, `feat:` | A space and a description must follow the colon | A conventional commit |
| `feat(): add` | A scope is a noun | A conventional commit with no scope |
| `feat (cli)!: add`, `feat !: add` | No space inside the prefix | A conventional commit |
| A `BREAKING CHANGE:` line before the last paragraph | A footer is in the last paragraph | Declares a breaking change |
| `BREAKING CHANGE:gone` | A space follows the colon | Declares a breaking change |
| `BREAKING CHANGE #12` | ` #` also separates a footer token from its value | Declares nothing |

## API

Go publishes the reading from `go.putnami.dev/protocol/cli`:

| Symbol | Purpose |
| --- | --- |
| `ConventionalCommit` | The type, the scope and the breaking declaration of one message |
| `ParseConventionalCommit` | Reads a subject and a body; `ok` is false for a subject that is not a conventional commit |

The reading has no document and no schema. Go is its only runtime:
`@putnami/cli-protocol` has no reader for it.

## Consumers

| Consumer | Use |
| --- | --- |
| `tooling/cli/internal/git` | The version bump (`BumpFor`) and the breaking flag of a release baseline |
| `tooling/cli/internal/commands/versioncmd` | The section a commit takes in the changelog |
| `go/extension/internal/jobs/apicheck` | The marker that allows an incompatible API or command-surface change in `validate` |

The bump policy is not here: which type advances which number, the pre-1.0
rule and the support-catalog rule belong to the CLI. See
[version management](../../../tooling/cli/doc/14-version-management.md).
