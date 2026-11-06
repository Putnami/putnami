# How `putnami observe` reads a Ruby or Rails repository

The Ruby detector finds the components of a Ruby repository, such as a Rails
modular monolith with packwerk packs and local engine gems, from its
`package.yml`, gemspec and `Gemfile` files. It runs no Ruby, Bundler, Rails or
packwerk: it reads the files as text. The change analyses use what it finds to
name the changed and dependent components, attribute tests and coverage, and
diff `Gemfile.lock`.

## Components

The detector walks the repository, past `.git`, `node_modules`, `vendor`,
`tmp`, `log`, `coverage` and the other hidden directories, such as `.bundle`.
A component is a directory, of ecosystem `ruby`:

| The directory holds | Source | Name |
|---|---|---|
| a `package.yml`, a packwerk pack | `workspace` | the gemspec's name, else the directory, as packwerk names the pack |
| a `*.gemspec`, a gem | `manifest` | the name the gemspec gives |
| a `Gemfile`, an application | `manifest` | none, unless a gemspec gives one |

The root of a Rails application, which holds its `Gemfile`, is the component
`.`, and the root `package.yml` of a packwerk application makes it a
`workspace` one. A file of a pack belongs to the pack, the innermost component
that holds it, not to the application.

## Edges

A pack depends on each pack its `package.yml` lists under `dependencies`, by
its path from the repository root, as packwerk names packs:

```yaml
# packs/checkout/package.yml
enforce_dependencies: true
dependencies:
  - packs/pricing
```

A component depends on the local gem its `Gemfile` names with a literal path,
resolved against the `Gemfile`'s directory, and on the gem of a `path` block
that lies at the block's path or below it:

```ruby
gem "admin", path: "engines/admin"

path "gems" do
  gem "audit_log"
end
```

A component also depends on the local gem its gemspec names in an
`add_dependency`, `add_runtime_dependency` or `add_development_dependency`.
An absolute path, a path outside the repository or to no component, a gem
of a registry or a Git source, and the component itself are no edge. A gem
entry whose options go on after a trailing comma is read as one line.

In a Rails application whose `checkout` pack lists `pricing`, a change to
`packs/pricing` marks `packs/checkout` dependent:

```text
  components  2 changed, 1 dependent, 1 unaffected
    changed   . [ruby, workspace]
    changed   packs/pricing [ruby, workspace]
    dependent packs/checkout [ruby, workspace], depends on packs/pricing
```

The application `.` is changed there because the change also bumps a gem in
its `Gemfile.lock`.

## Gemfile.lock

Each `Gemfile.lock` is a lockfile of ecosystem `gem`, and its dependency
changes name the component that holds it. The detector reads the specs of
its `GEM` and `GIT` sources, `name (version)`. A gem locked for several
platforms, such as `nokogiri (1.16.7-x86_64-linux)`, is one version, read
before the first `-` as Bundler reads it. The specs of a `PATH` source, the
repository's own gems, are left out. A version with a fourth number, such as
Rails's `7.2.2.1`, compares, so its bump is `upgraded`. A package URL of type
`gem` in an SBOM matches the change.

## Reports

Have RSpec write a JUnit report with
[rspec_junit_formatter](https://github.com/sj26/rspec_junit_formatter), and
SimpleCov a Cobertura one with
[simplecov-cobertura](https://github.com/jessebs/simplecov-cobertura):

```ruby
# spec/spec_helper.rb, before the application loads
require "simplecov"
require "simplecov-cobertura"
SimpleCov.start do
  formatter SimpleCov::Formatter::CoberturaFormatter
end
```

```bash
bundle exec rspec spec packs/*/spec engines/*/spec --format progress --format RspecJunitFormatter --out rspec.xml
```

rspec_junit_formatter writes each case's `file` relative to the directory
RSpec ran in, such as `./packs/pricing/spec/models/pricing/calculator_spec.rb`,
and simplecov-cobertura `coverage/coverage.xml` with each file relative to
SimpleCov's root. With Minitest,
[minitest-reporters](https://github.com/minitest-reporters/minitest-reporters)'
`JUnitReporter` writes `test/reports/TEST-*.xml`, each case with its `file`,
and [simplecov-lcov](https://github.com/fortissimo1997/simplecov-lcov) writes
`coverage/lcov/*.lcov`. Discovery finds all four by their content. A case's
file and a covered file are read relative to the report's component, to the
repository root, then to the report's directory, so a run at the root and a
run in one gem both land on the right components. See
[tests and coverage](06-change-analyses.md#tests-and-coverage).

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.ruby_package_invalid` | The `dependencies` of a `package.yml` are not a list of pack names: the pack keeps no edge from them. |
| `observe.ruby_walk_bounded` | The walk stopped after 200,000 entries or could not walk the repository. |

## Limits

- A `Gemfile` and a gemspec are read as text, never evaluated: a gem entry
  counts whatever condition surrounds it, a path or a name that a variable or
  an expression gives is no edge, nor is an `eval_gemfile` or a `gemspec`
  directive, and a gemspec whose name is not a literal string has no name.
- A gem name that two gemspecs give, such as a gem and its copy in a test
  fixture, is the first directory's in path order for a gemspec dependency,
  and the one within the block's path for a `path` block.
- A `vendor` directory is skipped at any depth, so a gem under `vendor/gems`
  is no component.
- A `Gemfile` at the root makes the root a `ruby` component, even in a
  repository whose other code is not Ruby, unless another detector names the
  root: a `go.mod`, `composer.json` or `pyproject.toml` there wins, as its
  detector runs before the Ruby one.
- A `dependencies` flow list written over several lines, such as
  `dependencies: [` on its own line, is the `observe.ruby_package_invalid`
  warning: write it as a block list.
- Edges from constant references in the code, which `packwerk check` reads,
  are not read, nor packwerk's own `packwerk.yml`.
- `gems.rb` and `gems.locked`, the other names Bundler accepts, are not read.
- A RubyGems pre-release version, such as `8.0.0.beta1`, does not compare: its
  bump is `changed`.
