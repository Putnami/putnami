# Run a PHP or Symfony repository with Putnami

`@putnami/php` turns each Composer component of a repository, such as a
Symfony application and the packages beside it, into a Putnami project, and
runs its lint and test with the PHPStan and PHPUnit the repository already
installs. It reads the repository the way `putnami observe` does, and it never
installs PHP, Composer or a tool.

## Adopt it

1. Declare the extension in `putnami.workspace.json`:

   ```json
   { "extensions": ["@putnami/php"] }
   ```

2. Run `putnami projects sync`. Each directory that holds a `composer.json`,
   outside `vendor/`, joins the workspace's `includes`. No `putnami.json` is
   needed: the extension names the component by its package, adds an edge to
   each package it requires through a path repository, and tags it `php`. An
   application whose `composer.json` names no package is named by its
   directory in the steps.
3. Run `composer install`, as the team already does, then
   `putnami lint,test`.

## Where the tools come from

The **stack root** of a component is the directory whose `composer install`
puts it and its tools in a `vendor/`:

- the workspace root, for the root `composer.json` and for each package a
  path repository of the root `composer.json` provides, such as
  `packages/pricing` under `"repositories": [{"type": "path", "url": "packages/*"}]`;
- the component's own directory, for any other component.

Each task runs the tools of its stack root's `vendor/bin`.

## What each task runs

Each task runs in the component's directory, with no shell. `{root}` is the
stack root and `{output}` the task's output directory.

| Task | Steps |
|---|---|
| `lint` | `{root}/vendor/bin/phpstan analyse --no-progress --no-interaction src` |
| `test` | `php -d xdebug.mode=coverage -d pcov.directory={root} -d pcov.exclude=~/vendor/~ {root}/vendor/bin/phpunit --log-junit {output}/junit.xml --coverage-clover {output}/clover.xml --coverage-filter src tests` |

There is no build task: PHP has no compile step.

PHPStan reads the `phpstan.neon`, `phpstan.neon.dist` or `phpstan.dist.neon`
of the component's directory, and analyses its `src` whatever `paths` the
configuration names. PHPUnit reads the `phpunit.xml`, `phpunit.dist.xml` or
`phpunit.xml.dist` of the component's directory, and runs its `tests`
directory, so the root application never runs a package's tests.
`--coverage-filter src` adds `src` to the sources the configuration names: a
root configuration that also names a package's sources reports them too, with
the lines the root's tests run. The test task needs a coverage driver, PCOV or
Xdebug. `-d xdebug.mode=coverage` switches Xdebug to coverage; the two `pcov`
settings make PCOV instrument the whole stack root but `vendor/`, where it
would otherwise instrument the `src` of the directory PHPUnit runs in.

The test task then reads the JUnit and Clover reports into the test and
coverage summary, even when PHPUnit exits non-zero, as it does on a failed
case. Each case names its test file and each covered file is a
file of the repository, so `putnami observe` places every case and every line
on its component. A failed case fails the task with `stacks.test_failed`; a
step that exits non-zero and wrote no failed case fails it with
`stacks.step_failed`; a missing report after steps that succeeded fails it with
`stacks.report_missing`. A task of a component waits for the same task of the
components it requires, and the components of one stack root run one task at
a time, because they share its `vendor/`.

## The toolchain check

Before its steps, each task compares the version `php --version` prints with
the version `.php-version` states, or else the range `require.php` of
`composer.json` states, in the component's directory, then at its stack root.
An exact version wins over a range wherever it is: a root `.php-version` of
`8.4.10` applies to a package whose own `composer.json` states `>=8.4`.

| Case | Result |
|---|---|
| `.php-version` states `8.3`, the machine has 8.3.6 | The task runs; `8.3` matches the 8.3 releases and keys the minor release |
| `require.php` states `>=8.4`, `^8.2` or `8.3.*`, and the machine's version is in it | The task runs; the key carries the range, so two machines in it share entries |
| The machine's version is outside the version or the range | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` to run anyway |
| `php` is not on `PATH` | The task fails with `stacks.toolchain_missing` |
| Nothing states a version | The warning `stacks.toolchain_unpinned`: the cache key carries no PHP version |

A range uses Composer's constraint syntax: `>=`, `<`, `!=`, `^`, `~`,
wildcards, hyphen ranges, a comma or a space for and, `||` for or. Inside a
range, `=8.3` means 8.3.0, as in Composer. To key one
PHP version, state it in `.php-version`. The `config.platform.php` of
`composer.json` is the version Composer resolves dependencies for, often the
lowest one supported, and is not read.

## What reaches the cache key

A task's key covers the component's files its task reads, the stack root's
`composer.json`, `composer.lock` and `.php-version`, the
component's `putnami.json`, the expected PHP version, and the key of the same
task of each component it requires.

| Input set | Patterns |
|---|---|
| `sources` | `src/**` |
| `tests` | `tests/**` |
| `app` | `config/**`, `templates/**`, `translations/**`, `migrations/**`, `assets/**`, `importmap.php` |
| `manifests` | `composer.json`, and the stack root's `composer.json` |
| `lintConfig` | `phpstan.neon`, `phpstan.neon.dist`, `phpstan.dist.neon`, `phpstan-baseline.neon`, `phpstan-baseline.php`, `.php-cs-fixer.php`, `.php-cs-fixer.dist.php` |
| `testConfig` | `phpunit.xml`, `phpunit.dist.xml`, `phpunit.xml.dist`, `.env`, `.env.test` |
| `lockfiles` | `composer.lock`, and the stack root's `composer.lock` |

`lint` reads `sources`, `manifests`, `lockfiles` and `lintConfig`: PHPStan
reads the types of the dependencies, so a `composer.lock` change reruns it.
`test` reads every set but `lintConfig`. A change to a test, a template or the
PHPUnit configuration reruns test only. Local files such as `.env.local` and
`.env.test.local` are no input: a run that depends on them is not shared.

## Override the profile

A component overrides any value of the profile in its own `putnami.json`,
under `options["@putnami/php"]`. An object merges; any other value, such as a
list of steps or patterns, replaces the profile's whole. The overrides a team
uses most:

Lint with PHP-CS-Fixer instead of PHPStan, or with both:

```json
{
  "options": {
    "@putnami/php": {
      "tasks": {
        "lint": {
          "steps": [
            ["{root}/vendor/bin/php-cs-fixer", "fix", "--dry-run", "--diff"],
            ["{root}/vendor/bin/phpstan", "analyse", "--no-progress", "--no-interaction", "src"]
          ]
        }
      }
    }
  }
}
```

Sources outside `src`, such as a `lib` PSR-4 root, for both tasks: override
the input set and the steps that name `src`.

```json
{
  "options": {
    "@putnami/php": {
      "inputs": { "sources": ["lib/**"] },
      "tasks": {
        "lint": { "steps": [["{root}/vendor/bin/phpstan", "analyse", "--no-progress", "--no-interaction", "lib"]] },
        "test": { "steps": [["php", "-d", "xdebug.mode=coverage", "-d", "pcov.directory={root}", "-d", "pcov.exclude=~/vendor/~", "{root}/vendor/bin/phpunit", "--log-junit", "{output}/junit.xml", "--coverage-clover", "{output}/clover.xml", "--coverage-filter", "lib", "tests"]] }
      }
    }
  }
}
```

Test without a coverage driver: keep the JUnit report and drop the Clover
one. `reports` is an object, so it merges: the empty `coverage` list replaces
the profile's.

```json
{
  "options": {
    "@putnami/php": {
      "tasks": {
        "test": {
          "steps": [["{root}/vendor/bin/phpunit", "--log-junit", "{output}/junit.xml", "tests"]],
          "reports": { "coverage": [] }
        }
      }
    }
  }
}
```

Run a PHP outside the stated range anyway: `"toolchain": {"check": "warn"}`.

A steps override keeps the profile's reports: a test step must still write
`{output}/junit.xml` and `{output}/clover.xml`, or the override also replaces
`tasks.test.reports`. An override fails the task with
`stacks.project_config_invalid` when it names an unknown member, changes `id`
or `markers`, or adds a stack-root input the extension does not declare. The
design of the stack core is in
[its design note](../../../tooling/stacks/doc/01-design.md).

## Limits

- A component whose `vendor/` belongs to a build other than the workspace
  root's or its own, such as a package another package's path repository
  provides: it is read as its own stack root, so its tasks look for the
  tools under its own `vendor/bin`.
- PHPStan and PHP-CS-Fixer are not chosen from what the repository installs:
  a repository without PHPStan overrides the lint steps.
- A component with no `src` or no `tests` directory overrides the steps that
  name it.
- An application whose `composer.json` sits at the workspace root, the usual
  Symfony layout, is not a project yet: `projects sync` does not add the
  workspace root, and `.` in `includes` is ignored. Its packages are.
- No publish: a package reaches Packagist through its tags.
