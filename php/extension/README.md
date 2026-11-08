# @putnami/php

`@putnami/php` runs the components of a Composer repository, such as a
Symfony application and the packages beside it, as Putnami projects:
`putnami lint` runs PHPStan and `putnami test` runs PHPUnit on each component
with Putnami's runner and cache, without changing the build.

It is a stack extension: a manifest, the PHP stack profile (`profile.json`)
and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The components and
their edges come from the Composer detector of `putnami observe`.

See [the reference](doc/01-php.md).
