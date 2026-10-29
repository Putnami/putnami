# ADR 0012 — The extension owns the Go steps of project creation and dependency edits

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`), the `project-setup` and
  `deps-edit` workspace lifecycle jobs

## Context

`putnami projects create` and `putnami deps add|remove|prune` ran Go steps in
the core CLI:

- `projects create` added a Go project to `go.work`, aligned its go directive,
  ran `go mod tidy`, and asked the module proxies for the
  `go.putnami.dev/app` version that a template variable rendered into
  `go.mod`.
- `deps add|remove|prune` edited `go.mod` with `go mod edit` and tidied the
  module.

The core is becoming a generic host that knows no language. Each of these
steps reads Go settings and runs the go command, which the Go extension
already resolves for its tasks: the pinned toolchain, the module caches, the
declared module origin and the netrc credentials.

## Decision

**The Go extension runs these steps in two workspace lifecycle jobs, and the
core CLI runs a job when the project's extension declares it.**

- `project-setup` takes `path`, the directory of the created project. It
  adds the project to `go.work`, creating it when missing, aligns the go
  directive, pins each `go.putnami.dev` requirement at `v0.0.0` to the
  version the channel names, and runs `go mod tidy`. With `stage: "prepare"`
  it only writes a missing `go.work`, the file the workspace lock pins Go
  from, and runs no go command: the core runs that stage first on a host
  where the Go the extension locks does not resolve, then pins and installs
  that Go.
- `deps-edit` takes `action` (`add`, `remove` or `prune`) and `edits`, one
  `{project, path, modules}` entry per module. It edits every module with the
  go arguments the core used, tidies each as the workspace build resolves it,
  and treats the edits of one run as one transaction. A go.work directive that
  pins every version of a module to a release applies to the tidy only for
  the versions no proxy serves, so a requirement a proxy serves keeps the
  version go.mod states.
- A Go template renders each framework requirement at `v0.0.0`, and
  `project-setup` rewrites it after the render. The template no longer needs
  a variable that only the core could compute.
- The jobs keep the core's go arguments, messages and progress lines. They run
  the go command the extension's tasks resolve, so the Go they use follows
  the workspace lock. The core resolves the toolchain before it runs a job, so
  the jobs install no Go: without one, a job emits one error that names
  `putnami install`.
- Both jobs refuse a hosted run, whose dependencies are offline: they reach
  the network and rewrite `go.work`, `go.mod` and `go.sum`.
- A `deps add` spec without a version that names the module a `go.work`
  member declares resolves from the tree, with no registry query. The module
  is required at the version `go.mod` requires it at, else at its major
  version's placeholder, and replaced by the member's directory, relative to
  the module, unless `go.mod` already replaces it. A spec with a version asks
  the registry. When the member cannot be told apart, because `go.work` does
  not parse, a member has no module path or two members declare the module,
  the add fails instead of guessing.

## Consequences

- The framework lookup reads the Go settings from `go env`, after the job's
  environment is set up, so the lookup and `go mod tidy` read one GOPROXY.
- A failed lookup now fails after the template renders. The project files
  stay, and the core names the create command to run again with `--force`.
- The go output of a failed step is quoted, bounded and redacted, in the
  job's error diagnostic, instead of streamed.

## Rejected alternatives

- **A template variable the extension computes before the render.** The
  template protocol has no hook that runs extension code before a render, and
  the version lookup needs the Go settings of the created project.
- **One job per action.** `deps prune` edits several modules, and one job
  keeps them in one transaction.
- **Ask the registry for every bare spec.** A workspace module that is not
  published yet cannot be added then, and a hand-written `require` and
  `replace` would bypass the transaction.
- **Match a spec against project names.** A `putnami.json` name is not a
  module path, so it could select the wrong module.
