# @putnami/moon

`@putnami/moon` runs the projects of a [moon](https://moonrepo.dev) workspace
as Putnami projects, in the mode the workspace adopted it in:

- **delegate**: `putnami lint`, `putnami test` and `putnami build` run
  `moon run <project>:<task>` on each affected project, read the reports its
  options name, and leave caching to moon;
- **import**: a project whose language has a Putnami extension runs that
  extension under Putnami's runner and cache for each task it runs with the
  same tool; every other task, and every other project, stays delegated to
  moon (D-070).

It is a delegated stack extension: a manifest, the moon stack profile
(`profile.json`), the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md#delegated-stacks), and a
workspace sync task that writes each project's `putnami.json` from the moon
project graph. The projects and their tasks come from the moon detector of
`putnami observe`.

See [the reference](doc/01-moon.md).
