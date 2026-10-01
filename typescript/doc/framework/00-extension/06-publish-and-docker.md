# Publish & Docker

The TypeScript extension contributes TypeScript-specific release artifacts. It owns the `npm` ecosystem profile — the coordinate and version rules, the shape of the workspace's `registries.npm` entry, and the job that reports each published member — and publishes to the `oci` ecosystem the extension SDK owns.

## Publish channels

Enable channels in project config:

```json
{
  "publish": ["npm", "archives"]
}
```

Common commands:

```bash
putnami build .
putnami publish .

putnami build .
putnami publish . --channel canary
```

Options include:

- `--channel <c[,c…]>` — the release-set channels this publication advances
- `--visibility <internal|private|public>` — the per-publication level of the declared chain
- `--access <public|restricted>`
- `--registry <url>` — an override for one publication
- `--dry-run`

There is no `--stable` and no `--also-branch-tag`. A version is derived from git: a commit tagged with its line's tag publishes that version, any other commit publishes the next version with an ordered pre-release suffix. A channel is advanced by the release, never by a publisher, so no dist-tag is written on any path.

## Registries

The npm endpoints come from the workspace's `registries.npm` entry, overridable per project:

```json
{
  "registries": {
    "npm": {
      "publish": "https://npm.example.com",
      "scopes": { "@acme": "https://npm.example.com" }
    }
  }
}
```

`publish` is where a publication uploads. Each `scopes` entry becomes one `@<scope>:registry=` line in the workspace `.npmrc`, and `upgrade --channel` reads each package's dist-tag on the registry its own scope names. No registry URL is hard-coded in the extension, and `--registry` is an override for a single command rather than the place a workspace declares its registry.

## Docker

When the `docker` publish channel is enabled, the extension generates a Dockerfile based on `oven/bun:slim` and copies the pre-compiled standalone bundle into the container.

```json
{
  "publish": ["npm", "docker"]
}
```

```bash
putnami build my-app --compile
putnami publish my-app
```

Docker options:

- `--docker-registry <registry>` — an **override**; the declared target is `registries.oci.publish`
- `--docker-tag <tag>`
- `--platform <platform>`
- `--port <number>`

The publisher pushes by digest and tags the version only. An image project yields an `oci` member of the release set, keyed by its repository path, so a workload's image travels in the same snapshot as the packages published beside it.

## Versions

Build output uses the version of the project's version line, derived from git: the line's last tag advanced by the conventional commits that touch it, plus `-<yyyymmddHHMMSS>-<sha>` on any untagged commit. Release a line with `putnami version tag --scope <line>`; see [Version Management](../../../../tooling/cli/doc/14-version-management.md).
