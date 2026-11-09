---
order: 15
---

# Package and Publish Commands

**Commands:** `putnami package [project] --docker` and `putnami publish [project] --docker`

**Purpose:** Build the OCI image of a Python application and push it to its
registry. A library packages and publishes nothing through this channel.

## Usage

```bash
putnami package <project> --docker [options]
putnami publish <project> --docker [options]
```

A project that declares the docker channel in its `putnami.json` gets the flag
from its configuration:

```json
{
  "options": {
    "publish": { "docker": true }
  }
}
```

## The image

`package --docker` assembles the image without a Docker daemon. It copies files
onto a base image and runs nothing inside it.

- **Base.** The official `python:<version>-slim` image, pinned by digest, of the
  Python minor that `uv python find` selects at the workspace root: the
  interpreter the tests run on. The extension pins Python 3.11 to 3.14. Another
  base must be named with its digest; a base with no digest fails.
- **Dependencies.** `uv export --frozen --no-dev` lists the production
  dependencies of `uv.lock`. `uv pip install --target` installs the registry ones
  as wheels only, checked against their locked hashes, for the image's platform
  and the Python release of its base. The local packages the export lists, the
  project when it has a build system and its workspace path dependencies, are
  copied to a staging directory and built into wheels there, so the repository
  is not written to. They go to `/opt/python/site-packages`, which `PYTHONPATH`
  names.
- **Application.** The project's files go to `/app`, without the `tests` and
  `test` directories, the `test_*.py`, `*_test.py` and `conftest.py` files,
  generated directories (`.venv`, `__pycache__`, `build`, `dist`, `.gen`), and
  the files that hold secrets (`.env`, `.env.*`, `.envrc`, `.pypirc` and
  `.netrc`). The staged copy that builds the local packages leaves out the same
  files.
- **Ignored build outputs.** A file under a directory named `dist`, `out` or
  `vendor` ships only when Git lists it: tracked, or untracked and not ignored.
  The cache key reads such a directory only through Git, so an ignored file
  there would change the image without changing its key. Outside a Git work
  tree, no file under such a directory ships.
- **Process.** The image runs `python /app/<entrypoint>`, with the `entrypoint`
  option of `package`, as the unprivileged user `65534`, with `PORT=8080`. The
  application reads its port from `PORT`.

A path dependency must be a dependency of the project in its `putnami.json`, so
that the image's cache key covers its files. Packaging fails with
`python.path_dependency_outside_closure` otherwise, and names the dependency to
add.

## Package options

- `--docker`: assemble the image.
- `--docker-base-image <image@sha256:...>`: replace the official base. It must
  provide `python` of the same minor on its `PATH`, with a glibc that runs
  `manylinux_2_28` wheels.
- `--entrypoint <path>`: the file the image runs, `src/main.py` by default. Like
  any option, it can be set in the workspace's or the project's options. Set it
  once in `options["@putnami/python"]` to give `serve`, `run` and the image the
  same file; the `python-server` template does.
- `--platform <platform>`: `linux/amd64` (default) or `linux/arm64`. An `arm64`
  image can be packaged on an `amd64` machine, because only wheels are installed.
- `--docker-tag <tag>`: add a tag to the content tag `c-<hash>`.
- `--docker-load`: also load the image into the local Docker daemon.
- `--dry-run`: record the docker channel without assembling the image.

The image lands in `.putnami/out/<project>/package/docker/`, as an OCI layout
with its `manifest.json`.

## Publish

`publish --docker` pushes the packaged image to the registry that
`registries.oci.publish` names, through the publisher the Go, TypeScript and PHP
extensions share. The workspace probe declares the image as an `oci` release-set
member of an application that publishes the docker channel, so a release set can
deploy it. `--docker-registry <registry>` overrides that registry for one push.

```bash
putnami publish <project> --docker --dry-run   # report what would be pushed
```

## Limits

- A registry dependency with no wheel for the image's platform fails packaging.
  Nothing is compiled during assembly.
- A workspace path dependency with native code is built for the machine that
  packages it.
- The base follows the Python minor of the machine, which the cache key does not
  cover. A workspace pins it with a `.python-version` file at its root, which
  the key covers.
- A module named like a test file (`test_*.py`) is left out, even when the
  application imports it.
- A `.pth` file in a dependency is not processed, because `PYTHONPATH` is not a
  site directory.
- A symbolic link in the application fails packaging.

See [ADR 0002](./adr/0002-an-application-image-installs-wheels-onto-the-official-python-image.md)
for the decision and the alternatives it rejects.
