# Publish & Docker

The TypeScript extension contributes TypeScript-specific release artifacts. It owns the `npm` ecosystem profile — the coordinate and version rules, the shape of the workspace's `registries.npm` entry, and the job that reports each published member — and publishes to the `oci` ecosystem the extension SDK owns.

## Package a service as an image

`putnami package` builds only the channels you turn on. With no channel on,
`putnami package --projects api` fails and names the option to set; under
`--all` or `--impacted` it warns and goes on. Turn the image channel on in the
project's `putnami.json`:

```json
{
  "name": "api",
  "options": {
    "package": {
      "docker": true,
      "port": 8080
    }
  }
}
```

Each key under `options.package` is the default of the `putnami package` flag
with the same name, so `"docker": true` is `--docker` and `"port": 8080` is
`--port 8080`.

```bash
putnami package --projects api --docker-load --docker-tag local
docker run --rm -p 8080:8080 -e DATABASE_URL='postgres://user:pass@host.docker.internal:5432/app?sslmode=disable' api:local
```

Add `--platform linux/arm64` on Apple silicon. The image holds the compiled
binary and the project's `.gen/public` assets, on a distroless base.

### The image port

The image sets `PORT` and exposes that port: 3000 unless you pass `--port` or
set `options.package.port`. `http()` listens on its `port` option when you pass
one, then on `PORT`, then on the `putnami.port` config value (3000). Leave the
port out of `http()` so the service follows `PORT`, and set
`options.package.port` to the port you publish. To change it at run time, pass
`-e PORT=8080`.

### What the compiled binary runs

`putnami build --compile` and the image compile one entry point. For an
application built on `@putnami/application`, it is a generated file,
`.gen/src/serve.bundled.ts`. That file runs the same entry as `putnami serve`:
`src/serve.ts`, or the file under `src/` the `./serve` export names. A project with neither
gets `bootstrapServe(app)` on the `app` that `package.json#main` exports
(`src/main.ts` by default).

The binary embeds the project's `conf/.env.yaml` and every
`conf/.env.<env>.yaml`, so it reads the same base and environment files as
`putnami serve`, `putnami.port` included, from any directory and in the
image, where the `PORT` the image sets outranks `putnami.port`. The
machine-local overrides `conf/.env.local.yaml` and `conf/.env.prod.yaml`, which
the workspace's `.gitignore` ignores, stay out of it. It reads `NODE_ENV` and
`APP_ENV` when it runs: the image sets `NODE_ENV=production`, so
`conf/.env.production.yaml` applies over `conf/.env.yaml`, and the framework
runs in production mode (no development error pages). With
`putnami.port: 8080` in `conf/.env.yaml`, the binary listens on 8080 wherever
it runs; the image's `PORT` follows [The image port](#the-image-port).

The binary embeds no `conf/.secrets.*` file. Pass secrets and per-deployment
values with environment variables: fields declared with `Env(...)`,
`${VAR}` references in the conf files, `CONFIG_DATA` (a YAML document), and
`DATABASE_URL` for the database. See
[Configuration](/docs/frameworks/typescript/configuration) and
[Persistence](/docs/frameworks/typescript/persistence#database-configuration).

To run the binary without Docker:

```bash
putnami build --projects api --compile
PORT=8080 .putnami/out/api/~build/compile/serve.bundled-darwin-arm64
```

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
