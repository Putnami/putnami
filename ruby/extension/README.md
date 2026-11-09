# @putnami/ruby

`@putnami/ruby` runs the components of a Ruby or Rails repository as Putnami
projects: `putnami lint` runs RuboCop and `putnami test` runs RSpec, or
Rails's Minitest runner, on each Rails application, packwerk pack and local
gem, with Putnami's runner and cache, without changing the build.

It is a stack extension: a manifest, the Ruby stack profile (`profile.json`)
and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The components and
their edges come from the Ruby detector of `putnami observe`.

See [the reference](doc/01-ruby.md).
