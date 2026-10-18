# Infra CLI

`putnami cloud infra plan [<project>]` prints the deployment plan of the
workspace workloads: the databases, events, storage, secrets, scheduled jobs and
runtime that each workload's aggregated infra manifest declares. The package is
`infracli` (`go.putnami.dev/cloud/extension/internal/infracli`).

```bash
putnami cloud infra plan                 # every application workload, sorted by name
putnami cloud infra plan my-app          # one workload
putnami cloud infra plan --output=json   # one result envelope
```

The command reads `<project>/.gen/requirements.json`, the manifest
`putnami build` writes, and runs no build. It does not load the workspace: the
CLI lists every workspace project, with its resolved `type`, in the
`workspaceProjects` member of the job context, and the command plans the
projects whose type is `application`. A project without a `type` comes from a
CLI that predates the member, so the command fails and says to upgrade the CLI.
A context without the member is refused the same way, while an empty member
means a workspace with no projects and prints `No workloads found.` It does not
read a missing type as `application`.

The output is the output of `putnami infra plan`, which the core CLI served
until it deleted the command: the same projects in the same order, the same
text, the same `data` in the result envelope, and the same exit codes. Two
things differ:

- The envelope's `command` is `cloud infra`.
- On a failure in structured mode, the core also printed `putnami: <message>`
  on stderr. The Cloud runtime prints only the failure envelope on stdout.
