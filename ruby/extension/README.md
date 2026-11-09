# @putnami/ruby

`@putnami/ruby` runs the components of a Ruby or Rails repository as Putnami
projects: `putnami lint` runs RuboCop and `putnami test` runs RSpec, or
Rails's Minitest runner, on each Rails application, packwerk pack and local
gem, with Putnami's runner and cache, without changing the build.

For an application, a component whose directory holds `config.ru`:

- `putnami serve` runs `rails server`, or `rackup` without `bin/rails`;
- `putnami run` runs `bundle exec ruby` on an entrypoint, `bin/rails` by
  default;
- `putnami package` assembles an OCI image on the official Ruby slim image,
  with no Docker daemon, on a Linux machine of the image's architecture;
- `putnami publish` pushes that image to the workspace's OCI registry.

It is a stack extension: a manifest, the Ruby stack profile (`profile.json`)
and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The components and
their edges come from the Ruby detector of `putnami observe`.

See [the reference](doc/01-ruby.md).
