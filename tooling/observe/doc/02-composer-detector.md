# How observe reads a Composer repository

The Composer detector finds the components of a PHP repository, such as a
Symfony application, from its `composer.json` files. It runs neither Composer
nor PHPUnit, and it reads nothing under `vendor/`.

## Components

Each directory that holds a `composer.json` is one component. The detector
skips `.git`, `vendor`, `node_modules` and hidden directories.

| Member | Read from |
|---|---|
| Name | The manifest's `name`. A Symfony application often has none. |
| Type | `application` when the manifest's `type` is `project`, `library` otherwise. |
| Version | The manifest's `version`, when it declares one. |

## Dependencies

A component depends on another when it requires that component's package, in
`require` or `require-dev`, and a path repository provides that package:

```json
{
  "require": { "demo/pricing": "^1.0" },
  "repositories": [{ "type": "path", "url": "packages/*" }]
}
```

The path repository's `url` is a directory or a glob pattern, relative to the
manifest that declares it. A package that a registry provides is not a
component, so requiring it creates no dependency. A path outside the
repository creates none either.

## Areas

An area is a part of a component. Each directory directly under a PSR-4 root
of the component (`autoload` and `autoload-dev`) is an area named by that
directory. `src/Controller` and `tests/Controller` are the two paths of one
`Controller` area. A manifest without a PSR-4 root uses `src/`.

A root that lies inside another component belongs to that component: when an
application autoloads `packages/pricing/src/`, that directory is a source of
the `packages/pricing` component.

A file belongs to the deepest component directory that holds it, and to the
area whose path holds it, or to no area.

## What the change analyses read

Each component also carries:

- `composer.lock` beside its manifest, the lockfile the dependency diff reads;
- its source roots and its test roots: the `autoload-dev` roots that are not
  also autoloaded, and the test suite directories of its PHPUnit
  configuration;
- its PHPUnit configuration (`phpunit.xml`, `phpunit.xml.dist` or
  `phpunit.dist.xml`) and the JUnit, Clover and Cobertura output files it
  names. A CI that passes `--log-junit` or `--coverage-clover` on the command
  line leaves them empty; report discovery still finds those reports by their
  content.

A `composer.json` or a PHPUnit configuration that does not parse is a
diagnostic. The detector skips it and continues.

## The probe wire

The detector answers as a workspace probe of the
[probe protocol](../../../protocols/extension/doc/07-lifecycles.md): one project
per component, its dependencies as directory paths, and the areas, roots,
lockfile and PHPUnit outputs in the project's metadata. It lives in
`@putnami/observe` until the CLI can run another extension's probe outside a
workspace; it can then move to its own extension with no contract change.
