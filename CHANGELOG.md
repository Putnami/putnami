# Changelog

## 0.3.0 — 2026-10-02

The `putnami` CLI, its extensions, the Go and TypeScript frameworks and the
sites move to 0.3.0. The protocols and Python lines move to 0.2.1. Each line's
own changelog lists its commits.

- The documented first-use path runs on a machine that holds only Bash, `curl`,
  `tar` and a SHA-256 tool. The TypeScript extension installs its pinned Bun and
  the Go extension its pinned Go under `~/.putnami/toolchains/`, once per
  machine. Lint no longer needs Node.js.
- A workspace that Git does not manage serves, tests and builds. `version`,
  `publish`, `deploy`, `--impacted` and `--baseline` refuse there with one line
  that names git.
- `putnami init` resolves the extensions, the template and the starter's
  dependencies on one channel: `--channel`, `PUTNAMI_CHANNEL`, or the channel
  the CLI was installed from.
- Go: the health, platform and push-delivery events plugins mount their routes
  on the application's HTTP server. The go-server starter answers 200 on
  `/_/health`.
- `@putnami/web`: server pages hydrate without React error 418, and pages
  above 12,800 bytes hydrate under the default content security policy.

### What you have to change

- **Go, an application that adds its own `/_/health` route** while it keeps the
  health plugin stops at configure on a duplicate route. Remove your route, or
  remove the plugin.
- **Go, an application with route plugins and zero or several HTTP server
  plugins** stops at configure with an error that names `RegisterOn`. Call
  `RegisterOn(server)` on each route plugin before configure.
- **The Go extension no longer installs Go per workspace.** The link
  `.putnami/extensions/@putnami-go/bin/go` is gone; Go lives under
  `~/.putnami/toolchains/go/go-<version>/`. Update any script that called the
  old path.

Rollback: `putnami pin 0.2.0` in a workspace, or
`putnami upgrade --version 0.2.0` for the machine-wide CLI.

## 0.2.0 — 2026-10-01

First public release. Every version line starts at 0.2.0: the `putnami` CLI and
its language extensions, the protocols, the Go and TypeScript frameworks, and
the project templates.

- [Install the CLI](tooling/cli/doc/22-installing-the-cli.md)
- [Release contract](RELEASE.md): supported platforms, channels, and
  compatibility before 1.0
