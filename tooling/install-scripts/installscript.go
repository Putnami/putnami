// Package installscript holds the tests of the CLI's install and smoke
// scripts: install.sh, install.ps1, smoke-check-release.sh and .ps1, and
// smoke-first-use.sh, run against stub registries and stub CLIs.
//
// They live in their own project because a cache key is per project and per
// command, never per task. Inside @putnami/cli they ran on every change to the
// CLI, although they read the scripts and the install documentation, not the
// CLI's code. Here the key holds those files and this package.
//
// The module requires no CLI module on purpose: a dependency edge would fold
// the CLI's sources into the key. The CLI's spec gate reads this project's
// proofs through its featureAuthority declaration instead.
package installscript

// lfPolicyAttributes is the .gitattributes line the release smoke requires. It
// is the CLI's git.LFPolicyAttributes, which a test of the CLI pins to the
// scripts.
const lfPolicyAttributes = "* text=auto eol=lf"
