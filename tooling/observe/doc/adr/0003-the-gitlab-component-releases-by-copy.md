# ADR 0003 — The GitLab component releases by copy

- **Status**: accepted
- **Date**: 2026-11-23
- **Scope**: `@putnami/observe` (`gitlab/`), feature
  `tooling/observe-gitlab-component`

## Context

GitLab users find and trust a CI step through the GitLab CI/CD catalog. The
catalog lists a component only from a gitlab.com project marked as a catalog
project, whose root holds a `README.md` and the component under `templates/`,
and only for a release a job of that project's own pipeline creates with the
`release` keyword on a semantic version tag. A pipeline then includes it with
`include: component: gitlab.com/<project>/<component>@<version>`, where `@1`
selects the newest `1.x.y` the catalog published. The component runs
`putnami observe`, whose flags, docs and tests live in this repository, as
the GitHub action of [ADR 0001](0001-the-observe-action-releases-by-copy.md)
does.

A component is one YAML file: it ships no script beside it, so the job's
script is inline in the template.

## Decision

The component's source is `tooling/observe/gitlab/`: `templates/observe.yml`,
its `README.md`, and `project.gitlab-ci.yml`, the catalog project's own
pipeline. The gate of this repository runs the template's script against a
fake CLI, so the component and the observe docs change in one pull request.

The gitlab.com project `putnami/observe` holds only releases. `release.sh`
replaces its root with the component, its README, its pipeline as
`.gitlab-ci.yml` and the licence, tags `X.Y.Z`, with no `v` since the catalog
reads semantic versions, and moves the branch and the tag in one atomic push.
It refuses a version that is not `X.Y.Z` or that exists. The tag pipeline of
the project creates the release, which publishes the version to the catalog.

No workflow of this repository pushes to gitlab.com: an owner runs
`release.sh` against a clone of the project with their own gitlab.com
credentials, so this repository holds no gitlab.com token. A moving major
tag is not needed: the catalog resolves `@1` itself.

The job's `id_tokens` declares one ID token, for the auth server's audience
only, and the script keeps the tokens it hands to `putnami observe`, and the
job token, away from the installer, as the action's script does.

## Rejected alternatives

- The source lives in the gitlab.com project with its own CI. Nothing in this
  repository's gate would test it, and a change to `putnami observe` and its
  component would take two changes in two code hosts.
- A GitHub workflow pushes the release to gitlab.com, as the action's release
  workflow does. It would store a gitlab.com token with write access in this
  repository for a step an owner takes a few times a year.

## Consequences

- A contributor never edits `putnami/observe` on gitlab.com by hand: the next
  release replaces its root.
- Creating the project, marking it as a catalog project and giving it a
  description are web steps an owner takes once, before the first release.
- A release that breaks an input or a default takes the next major version.
