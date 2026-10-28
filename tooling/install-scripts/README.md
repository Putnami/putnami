# @putnami/install-scripts

The tests of the CLI's install and smoke scripts, which live in
[`tooling/cli/scripts`](../cli/scripts): `install.sh`, `install.ps1`,
`smoke-check-release.sh`, `smoke-check-release.ps1` and `smoke-first-use.sh`.
They run each script against stub registries and stub CLIs, and check the install
documentation against the scripts.

## Why this is a separate project

A project declares cache-key inputs per **command**, never per task. Inside
`@putnami/cli`, these tests ran on every change to the CLI, although they read
the scripts and the install documentation, not the CLI's code.

Here the key holds what the tests read (`options.test.filePatterns`): the
scripts, the two install pages, and the two sources `install.ps1` mirrors, the
CLI's version switch and the SDK's Windows file lock.

The module requires no CLI module, so the project has no dependency edge to the
CLI: an edge would fold the CLI's sources into the key. The tests still prove
checks of four CLI features (`cli/first-use-path`, `cli/windows-consumers`,
`cli/context-discovery` and `cli/init-channel`). The CLI's spec gate reads
those proofs because `featureAuthority.owner` names `cli/first-use-path`
([ADR 0081](../cli/doc/adr/0081-a-feature-authority-project-attests-for-the-spec-owner.md)).

The CLI pins one value to the scripts itself: a test of `tooling/cli/internal/git`
checks that `smoke-check-release.ps1` requires the CLI's line-ending policy.
