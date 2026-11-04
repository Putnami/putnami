# Observe

`@putnami/observe` reads what a CI job already produced, in a repository that
keeps its own CI and has no Putnami workspace. Its `putnami observe` command
runs at the end of the job, after the build and the tests, and changes nothing
in the pipeline.

It answers five questions:

- Which CI ran the job, and which repository and commit it built. The
  repository key is the code host and its numeric project id, which survive a
  rename or a transfer.
- Which commit the change is measured against, even on a shallow clone.
- Which files the change touches, which components they belong to, and which
  components depend on those.
- Which tests passed or failed and which lines they covered, per component,
  and how much of the changed lines they covered.
- Which dependency versions the lockfiles moved and, from the software bills
  of materials (SBOM) the CI wrote, their licences and the known
  vulnerabilities each change fixes or introduces.

A missing or malformed report, or a base it cannot reach, is a diagnostic:
`putnami observe` exits 0 whatever it observes, 2 on a usage error, and 1 when
it cannot read its job context or write its output. It needs Git 2.29 or
later.

## Use it

Install the extension in the user scope, then run it in the job:

```bash
putnami extensions install --user @putnami/observe
putnami observe
```

See [Observe a change in your CI](https://putnami.dev/docs/how-to/observe-a-change-in-your-ci)
for the GitHub Actions step and the GitLab CI job that install the CLI and the
extension, run `putnami observe`, and keep the evidence document as an
artifact.

On GitHub Actions and GitLab CI, nothing else is needed. In another CI, name
the base:

```bash
putnami observe --base origin/main
```

`--output=json` prints one JSON document instead of the summary.
`--evidence <path>` also writes the change evidence document, a CI artifact a
reporter can upload. `--publish` posts a commit status and one summary note on
the pull or merge request, which the next run updates in place.

See [doc/01-observe.md](doc/01-observe.md) for what each CI provides, how the
base is found and how reports are discovered.
See [doc/03-nx.md](doc/03-nx.md) for how it reads the projects of an Nx
workspace.
See [doc/04-node-workspaces.md](doc/04-node-workspaces.md) for how it reads the
packages of an npm, pnpm or yarn workspace.
See [doc/05-go-modules.md](doc/05-go-modules.md) for how it reads the modules,
edges and dependency versions of a Go repository.
See [doc/07-python.md](doc/07-python.md) for how it reads the projects and
edges of a Python repository, from a uv workspace or `pyproject.toml` files.
See [doc/06-change-analyses.md](doc/06-change-analyses.md) for how it maps a
change to components, tests, coverage and dependencies.

## Support

`@putnami/observe` is `experimental` in the
[support catalog](../../putnami.support.json): it promises no compatibility
yet, and no consumer in this repository runs it.
