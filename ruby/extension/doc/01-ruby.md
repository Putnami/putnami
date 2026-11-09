# Run a Ruby or Rails repository with Putnami

`@putnami/ruby` turns each Rails application, packwerk pack and local gem of
a Ruby repository into a Putnami project, and runs its lint and test with the
tools the team already uses: RuboCop, RSpec or Rails's Minitest runner, and
SimpleCov. It reads the repository the way `putnami observe` does. The
machine needs no Ruby: `putnami install` pins one when the machine has none
that fits.

## Adopt it

1. Declare the extension in `putnami.workspace.json`:

   ```json
   { "extensions": ["@putnami/ruby"] }
   ```

2. Run `putnami projects sync`. Each directory below the workspace root
   that holds a `Gemfile`, a packwerk `package.yml` or a `*.gemspec` joins
   the workspace's `includes`. No `putnami.json` is needed: the extension
   names the component by its gem, else by its pack's path; adds an edge to
   each pack its `package.yml` lists and each local gem its `Gemfile` or
   gemspec names; and tags it `ruby`.
3. Run `putnami install`, then `putnami lint,test`. `putnami install` pins
   Ruby, then installs the bundle of each stack root whose `bundle check`
   fails.

The [Ruby detector](../../../tooling/observe/doc/16-ruby.md) says which
directories are components and how their edges are read.

## What each task runs

A component's **stack root** is the nearest directory, from its own up to the
workspace root, that holds a `Gemfile`: the application's for a pack, the
gem's own for a gem with a `Gemfile`. `{project}` is the component's
directory and `{output}` the task's output directory.

| Task | Runs in | Steps |
|---|---|---|
| `lint` | the component | `bundle exec rubocop` |
| `test` | the stack root | `bundle exec rspec {project}/spec --format progress --format RspecJunitFormatter --out {output}/junit.xml` |
| `test`, Minitest | the stack root | `bundle exec rails test {project}/test --junit --junit-filename={output}/junit.xml` |

A component runs RSpec when it holds a `spec/**/*_spec.rb` file, Rails's
Minitest runner when it holds a `test/**/*_test.rb` file and no spec file, and
no test, which passes, when it holds no Ruby file under `spec/` or `test/`. A
component with Ruby test files that match neither runner, such as the
`test/test_*.rb` files of `bundle gem`, runs RSpec, which fails without a
`spec/` directory: name its steps in an override. The probe names this
choice, the component's `rspec`, `minitest` or `untested` variant, so it
reaches the cache key through the project metadata; the workspace adapter
hashes the Ruby files under `spec/` and `test/`, so adding or removing one
asks the probe again. Each command reads them to decide whether the
workspace changed: about half a second for 18,000 spec files.
Tests run from the stack root because a pack's specs load the application's
`.rspec`, `spec_helper` or `test_helper`. There is no build task: Ruby has no
compile step, and a gem reaches its registry through `gem push`.

The bundle needs the gems the steps name:

```ruby
group :development, :test do
  gem "rubocop-rails-omakase", require: false # or rubocop
end

group :test do
  gem "rspec_junit_formatter"                 # with RSpec
  gem "minitest-junit"                        # with Minitest
  gem "simplecov", require: false
  gem "simplecov-cobertura", require: false
end
```

and SimpleCov a Cobertura report, started before the application loads:

```ruby
# spec/spec_helper.rb or test/test_helper.rb
require "simplecov"
require "simplecov-cobertura"
SimpleCov.start do
  formatter SimpleCov::Formatter::CoberturaFormatter
end
```

The test task reads the JUnit report and `coverage/coverage.xml` at the stack
root into the test and coverage summary. A failed case, a step that exits
non-zero, or a missing report fails the task. The coverage summary counts
only the component's own files: those under its directory, outside a nested
pack or gem. A covered file in no component is left out with the warning
`stacks.coverage_unmapped`. The tasks of the components of one stack root
run one at a time, lint included, because they share its `coverage/`
directory, and a report another run left there is removed before the steps.

## Where Ruby and Bundler come from

`putnami install` pins one Ruby release for the workspace in
`putnami.lock.json`, and downloads it when the machine has no such Ruby. A
workspace with no Ruby component pins nothing.

| Statement | Pinned release |
|---|---|
| None | 4.0.9, the newest |
| A `.ruby-version` or `.tool-versions` that names a version, such as `3.3.6` | That release |
| One that names a minor, such as `3.4` | The newest release of that minor: 3.4.12 |
| A component's own statement narrows those | The same choice among the releases every component accepts; with no common release, the root's choice |
| A release the extension ships, already in the lock and still accepted | That release |
| A version the extension does not ship, such as `3.4.0` or `3.1` | No pin: the machine's Ruby runs |

The extension ships every stable release of Ruby 3.2, 3.3, 3.4 and 4.0 that
release `20261023` of
[`spinel-coop/rv-ruby`](https://github.com/spinel-coop/rv-ruby), a third
party that builds Rubies to run from any directory, holds: 3.2.1 to 3.2.11,
3.3.0 to 3.3.13, 3.4.1 to 3.4.12, and 4.0.0 to 4.0.9. Each archive is pinned
by SHA-256. `releases.go` lists them; `scripts/rv-ruby-releases.sh <tag>`
writes it again from another rv-ruby release. The pins cover macOS and Linux, each on arm64 and x86_64. Windows has
no archive, so it gets no pin. On Linux the pin is the glibc build, which does
not run on a musl Linux such as Alpine: put a Ruby on `PATH` there.

A `ruby` on `PATH` that satisfies the statement always runs, and so does any
`ruby` on `PATH` when nothing states a version. The pinned Ruby runs only when
`ruby` is missing or outside the statement. It then comes first on `PATH`, so
its own `bundle` and `gem` run too. `bundle install` then installs the gems in
the pinned Ruby's folder under the Putnami home, unless the repository sets
`BUNDLE_PATH`.

Bundler ships with Ruby, so the extension downloads no Bundler. When
`Gemfile.lock` states another Bundler, the install step runs `bundle install`,
which installs the locked Bundler and switches to it, so later tasks run that
one. The Bundler check only warns, because the stack core runs it before the
install step's own command, which would otherwise never run to fix it.

## Install the bundle

The extension installs the gems with Bundler.

| Job | What it does |
|---|---|
| `install`, before `lint` and `test` | Runs `bundle check` at the component's stack root, then `bundle install` when the bundle is incomplete or when `bundle --version` is not the `BUNDLED WITH` of `Gemfile.lock`, one run per stack root at a time. |
| `workspace-install`, from `putnami install` | Runs the same at each stack root of the workspace's projects, except the projects `disable.tags` excludes; `--force` installs a root whose bundle and Bundler pass as well. With no Ruby on the machine it warns and passes. |
| `deps-upgrade` | Runs `bundle update` at each stack root. |

With offline dependencies, such as in a hosted run, `bundle install --local`
installs from the gems already on the machine, a Bundler mismatch alone
installs nothing, and `deps-upgrade` fails.
There is no `workspace-fetch`: Bundler downloads a gem only as it installs
it, and `bundle cache` writes `vendor/cache` into the repository.

## Filter the tests and check the links

`putnami test --test <filter>`, short `-t`, passes the filter to RSpec as
`-e <filter>`, which runs the examples whose description holds it, and to
`rails test` as `-n /<filter>/`, quoted as a regular expression. A component
in which it selects no test passes. The filter is part of the task's key, so
a filtered result never answers an unfiltered run. A test step an override
names for another runner gets no filter.

`putnami lint` also checks every relative link and anchor of the component's
`README.md` files and `doc/` trees, as the other language extensions do; a
broken one fails it. `--docs-links=false` skips the check.

## The Ruby and Bundler checks

Before its steps, each task compares `ruby --version` with the version that
`.ruby-version` (`3.3.6` or `ruby-3.3.6`) or the `ruby` line of
`.tool-versions` states, and `bundle --version` with the `BUNDLED WITH`
version of `Gemfile.lock`, in the component's directory, then at its stack
root.

| Case | Result |
|---|---|
| The versions match | The task runs; a `3.3` version matches 3.3.0 and 3.3.6 |
| The Ruby versions differ, or `ruby` is not on `PATH`, and the workspace pins a release that matches | The task runs with the pinned Ruby |
| The Ruby versions differ, and no pinned release matches | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` to run anyway |
| The Bundler versions differ | The warning `stacks.toolchain_mismatch`; set `toolchain.also[0].check` to `fail` to fail the task |
| `ruby` or `bundle` is not on `PATH`, and no pinned release matches | The task fails with `stacks.toolchain_missing` |
| No file states an exact version | The warning `stacks.toolchain_unpinned`: the cache key carries no version for that tool |

## What reaches the cache key

A task's key covers the component's files its task reads, the stack-root
files, the component's `putnami.json`, the expected Ruby and Bundler
versions, and the key of the same task of each component it depends on.

| Input set | Patterns | Read by |
|---|---|---|
| `ruby` | the files RuboCop inspects by default: `**/*.rb`, `**/*.rake`, `**/*.gemspec`, `**/*.ru`, `**/*.jbuilder`, `**/.simplecov`, `**/Gemfile`, `**/Rakefile` and the other names of RuboCop's `AllCops.Include`, with `bin/*` and `exe/*` | lint |
| `rubocop` | `**/.rubocop*.yml` in the component, `.rubocop.yml` and `.rubocop_todo.yml` at the stack root | lint |
| `component` | `**`: every file of the component | test |
| `boot` | at the stack root, what the booted application reads: `Gemfile`, `.rspec`, `.simplecov`, `app/**`, `lib/**`, `config/**`, `db/**/*.rb`, `db/**/*.sql`, `spec/**` and `test/**`, less `spec/examples.txt`, `spec/reports/` and `test/reports/`, which a run writes | test |

Every set leaves out `vendor/`, `node_modules/` and `tmp/`, and `component`
also `log/`, `coverage/`, `storage/`, `.bundle/`, Putnami's `.gen/`, `db/*.sqlite3*`,
`public/assets/`, `public/packs/`, `.rspec_status` and `spec/examples.txt`:
what a run or an install writes is no input. An exclusion never leaves out
the component's `putnami.json`, `.ruby-version`, `.tool-versions` or
`Gemfile.lock`; the Ruby files under `spec/` and `test/`, which decide the
variant, reach the key through the probe's metadata. `.ruby-version`, `.tool-versions` and `Gemfile.lock`, in the component
and at the stack root, are inputs of every task: the lockfile pins RuboCop as
well as the test gems. Lint reads the files RuboCop inspects, not a fixture,
an ERB view or `.rspec`, so a change to them reruns test only. A pack's test
reads the application's own code at the stack root, since its specs boot the
application: a change there reruns the tests of every pack.

## Override the profile

A component overrides any value of the profile in its own `putnami.json`,
under `options["@putnami/ruby"]`. An object merges, `null` removes a value,
such as a task, and any other value, such as a list of steps or patterns,
replaces the profile's whole. The override applies after the component's
variant, and cannot change `variants`.

```json
{
  "options": {
    "@putnami/ruby": {
      "tasks": {
        "lint": { "steps": [["bundle", "exec", "rubocop", "--parallel"]] },
        "test": { "steps": [["bundle", "exec", "rake", "test", "TESTOPTS=--junit --junit-filename={output}/junit.xml"]], "cwd": "{project}" }
      },
      "tools": { "bundler": { "check": "warn" } }
    }
  }
}
```

Common overrides: a gem without Rails tests with `rake test` in its own
directory, as above, where a component with no Ruby file under `spec/` or
`test/` has no test task left to merge into, so the override names the whole
task, its `inputs` and `reports` included; more inputs, by replacing an input
set whole, with the profile's exclusions copied, since a list replaces the
profile's; a component that skips RuboCop with
`"tasks": { "lint": null }`; a repository whose SimpleCov writes its
report elsewhere replaces `tasks.test.reports`. A steps override keeps the
profile's reports: a test step must still write `{output}/junit.xml` and
`coverage/coverage.xml`. An override fails the task with
`stacks.project_config_invalid` when it names an unknown member, changes `id`,
`markers` or `variants`, or adds a stack-root input the extension does not
declare. The
design of the stack core is in
[its design note](../../../tooling/stacks/doc/01-design.md).

## Limits

- The CLI makes no project at the workspace root yet, so the application
  whose `Gemfile` sits there is no Putnami project: its packs and gems are,
  and its own specs do not run. Its files still reach every pack's test key.
- An application below the workspace root, such as `apps/shop`, is a
  component, and each of its packs depends on it, so its files reach every
  pack's key through that edge (ADR 0002 of the stack core). A local gem the
  application's `Gemfile` names and whose stack root is the application is
  left out with `stacks.stack_root_unsupported`: the edge would close a
  cycle.
- The application's lint, once it is a project, runs RuboCop at the root,
  which also inspects its packs and gems: they are inputs of its lint, beside
  each pack's own.
- SimpleCov merges a run with the earlier one of another command name: a
  stack root whose components mix RSpec and Minitest can count a line one
  runner covered in the other's report.
- An RSpec `example_status_persistence_file_path` other than `.rspec_status`
  or `spec/examples.txt` is written by each run and is a test input, so the
  test task never hits the cache: keep it under `tmp/`.
- Lint reads the component's `.rubocop*.yml` files and the stack root's, not
  a configuration in a directory between them nor a file `inherit_from`
  names elsewhere, nor a Ruby script without an extension outside `bin/` and
  `exe/`.
- The stack-root inputs are files at the workspace root, so a gem with a
  `Gemfile` of its own also reruns its test when the application's code
  there changes.
- `gems.rb` and `gems.locked`, the other names Bundler accepts, are not read.
- JRuby and TruffleRuby: the toolchain check reads `ruby --version` as CRuby
  prints it, so their tasks fail with `stacks.toolchain_missing`.
- `putnami lint,test --impacted` selects a pack from its own files and its
  edges, not from the stack-root files its test reads: a change only to the
  application's `app/` or `spec/` selects no pack, though its key moves. Use
  `--all` or name the projects.
- The install step runs `bundle check` before each task of each component,
  about a third of a second, then `bundle --version` when the check passes,
  one at a time per stack root, even when the task's result is cached.
- `--watch` matches the stack-root inputs without their exclusions, so a
  repository that keeps `spec/examples.txt` reruns its packs' tests after
  each run.
- No build or publish task.
