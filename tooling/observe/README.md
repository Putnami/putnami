# Observe

`@putnami/observe` reads what a CI job already produced, in a repository that
keeps its own CI and has no Putnami workspace. Its `putnami observe` command
runs at the end of the job, after the build and the tests, and changes nothing
in the pipeline.

It answers four questions:

- Which CI ran the job, and which repository and commit it built. The
  repository key is the code host and its numeric project id, which survive a
  rename or a transfer.
- Which commit the change is measured against, even on a shallow clone.
- Which files the change touches.
- Which test and coverage reports the job produced.

A missing or malformed report, or a base it cannot reach, is a diagnostic:
`putnami observe` exits 0 whatever it observes, and 2 only on a usage error.

## Use it

Install the extension in the user scope, then run it in the job:

```bash
putnami extensions install --user @putnami/observe
putnami observe
```

On GitHub Actions and GitLab CI, nothing else is needed. In another CI, name
the base:

```bash
putnami observe --base origin/main
```

`--output=json` prints one JSON document instead of the summary.
`--evidence <path>` also writes the change evidence document, a CI artifact a
reporter can upload.

See [doc/01-observe.md](doc/01-observe.md) for what each CI provides, how the
base is found and how reports are discovered.
