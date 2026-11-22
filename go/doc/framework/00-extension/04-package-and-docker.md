# Package & Docker

The Go extension can produce release artifacts for Go projects, including Docker images. It owns the `go` ecosystem profile — the module-path and version rules, the shape of the workspace's `registries.go` entry, and the job that reports each published member — and publishes to the `oci` ecosystem the extension SDK owns.

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
docker run --rm -p 8080:8080 api:local
```

Add `--platform linux/arm64` on Apple silicon. `package` compiles its own
binary, so it needs no prior `putnami build`. To pass the database connection
to the container, see
[Connect at run time](/docs/frameworks/go/persistence#connect-at-run-time).

### The image port

The image sets `PORT=3000` unless you pass `--port` or set
`options.package.port`. The Go HTTP server listens on `PORT` when it is set,
whatever `http.ServerConfig.Port` says. So a service that listens on 8080
outside the image listens on 3000 inside it. Do one of these:

- Set `options.package.port` to the port you publish, as above.
- Override it at run time: `docker run -e PORT=8080 -p 8080:8080 ...`.

The complete flag list is in the
[package reference](../../../extension/doc/package.md#flags).

## Docker

When the `docker` publish channel is enabled, the Go extension assembles a digest-pinned `gcr.io/distroless/static:nonroot` image around the statically linked binary.

```json
{
  "publish": ["docker"]
}
```

```bash
putnami build api --target linux/amd64
putnami publish api
```

Options include:

- `--docker-registry <registry>` — an **override**; the declared target is `registries.oci.publish` in `putnami.workspace.json`
- `--docker-tag <tag>`
- `--platform <platform>`

The publisher pushes by digest and tags the version only. Channel tags are written by the release projection, never by the publisher, so a repeated publish of the same content is idempotent.

## Registries

The Go endpoints come from the workspace's `registries.go` entry, overridable per project:

```json
{
  "registries": {
    "go": {
      "origin": "https://go.example.com",
      "proxy": ["https://proxy.golang.org", "direct"]
    }
  }
}
```

`origin` is the vanity module server a publication uploads to and `upgrade --channel` resolves `@v/<channel>.info` on, **per module** — one channel carries a different version per module, so each module is pinned to its own. `proxy` is the ordered `GOPROXY` list and never leads with the origin. `GO_REGISTRY_URL` stays a one-off override. There is no `options.publish.goRegistryUrl`.

## Archives and module publishing

Go packaging prepares platform archives and Go module source for release flows. A publication uploads the module zip to the origin, verifies the stored zip's digest, and reports the module as a published member of the release set; a `409 Conflict` whose stored zip matches the local digest is a reuse, not a failure, which is what makes a repeated tagged publish idempotent.

Versions are derived from git for the version line the project belongs to; there is no `--stable`. Use Putnami build output as the source of truth for release artifacts rather than rebuilding manually in deployment scripts.
