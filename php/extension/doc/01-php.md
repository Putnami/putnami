# Run a PHP or Symfony repository with Putnami

`@putnami/php` turns each Composer component of a repository, such as a
Symfony application and the packages beside it, into a Putnami project, and
runs its lint and test with the PHPStan and PHPUnit the repository already
requires. It reads the repository the way `putnami observe` does. The machine
needs neither PHP nor Composer.

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
3. Run `putnami install`, then `putnami lint,test`. The first task of a
   component runs `composer install` at its stack root.

## Where PHP and Composer come from

`putnami install` pins one PHP release for the workspace in
`putnami.lock.json`, and downloads it when the machine has no such PHP. A
workspace with no `composer.json` outside `vendor/` pins nothing.

| Root statement | Pinned release |
|---|---|
| None, or a range that accepts 8.4.23 | 8.4.23 |
| A range or a `.php-version` that refuses 8.4.23 | The highest of 8.3.32, 8.4.23 and 8.5.8 it accepts |
| A release the extension ships, already in the lock and still accepted | That release |
| No shipped release is accepted | No pin: the machine's PHP runs |

The Linux and macOS archives come from `dl.static-php.dev`, a third party, and
the Windows archives from `windows.php.net`. Each archive is pinned by
SHA-256. Windows on ARM has no archive, so it gets no pin.

A `php` on `PATH` that satisfies the statement always runs. The pinned PHP
runs only when `php` is missing or outside the statement.

Composer is a single file, so the extension downloads `composer.phar` 2.11.1
under the Putnami home and checks its SHA-256. The `install` step of each
`lint` and `test` runs `composer install` at the component's stack root. It
skips a stack root whose `vendor/composer/installed.json` is newer than
`composer.json` and `composer.lock`. A run with offline dependencies installs
nothing.

The pinned PHP holds neither PCOV nor Xdebug. On a PHP with no coverage
driver, `test` runs PHPUnit with its JUnit report only, reads no Clover report
and warns with `stacks.coverage_unavailable`. For coverage, put a PHP with a
driver on `PATH`.

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
the lines the root's tests run. Coverage needs a driver, PCOV or
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
| The machine's version is outside it, or `php` is not on `PATH`, and the workspace pins a release in it | The task runs with the pinned PHP |
| `php` is not on `PATH` and the workspace pins no release | The task fails with `stacks.toolchain_missing` |
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

Test without coverage, even on a PHP with a driver: keep the JUnit report and
drop the Clover one. `reports` is an object, so it merges: the empty `coverage` list replaces
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
