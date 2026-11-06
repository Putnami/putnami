# How `putnami observe` reads a Pants repository

A repository whose root holds `pants.toml` is a Pants repository. Pants infers
most dependencies from imports, so no static read of the repository sees them:
the Pants detector reads the export the CI wrote, and never runs `pants`. The
change analyses use what it finds to name the changed and dependent
components, attribute tests and coverage, and diff the resolve lockfiles.

## The export

Write the export in the CI, before `putnami observe`:

```bash
pants peek :: > pants-graph.json
```

`putnami observe` reads `pants-graph.json` at the repository root, or the file
`--pants-graph` names, resolved against the directory you run in. `peek` lists
every target, the targets Pants generates for each file included, and each
target's `dependencies`: the dependencies Pants expanded from the BUILD files
and inferred from imports, with no flag needed from Pants 2.20 on. Keep the
export out of the commit, as a file the job writes.

## Components

A component is a directory, of ecosystem `pants`. Each target belongs to the
directory of the BUILD file that declares it or its generator, so
`src/python/pricing:pricing`, `src/python/pricing:tests` and the file target
`src/python/pricing/pricing.py` are one component `src/python/pricing`. A
target generated for a file in a subdirectory, such as
`src/python/pricing/rules/vat.py:../pricing`, belongs to the directory of its
BUILD file, `src/python/pricing`. `//` is the root. With the export, the
component's source is `orchestrator`.

A third-party target is no component: `python_requirement`,
`python_requirements`, `poetry_requirements`, `pipenv_requirements`,
`pants_requirements`, `go_mod`, `go_third_party_package`, `jvm_artifact` and
`node_third_party_package`. Neither is an internal target, whose type starts
with `_`, such as a resolve's `_lockfiles`. A directory such as
`3rdparty/python`, which holds only requirements, is therefore no component,
and a file in it, such as a lockfile, belongs to none.

## Edges

Each address in a target's `dependencies` is an edge from the target's
component to the dependency's component, when the two differ. A dependency on
a third-party or internal target is no edge, nor one on the component itself.
For example, when `src/python/api/main.py` imports `pricing.pricing`, the
export lists `src/python/pricing/pricing.py` among its dependencies, and the
component `src/python/api` depends on `src/python/pricing`:

```text
  components  1 changed, 1 dependent, 1 unaffected
    changed   src/python/pricing [pants, orchestrator]
    dependent src/python/api [pants, orchestrator], depends on src/python/pricing
```

## Without the export

Without an export, each directory whose `BUILD` or `BUILD.*` file declares a
target other than a third-party or internal one is a component of source
`workspace`, with no edge. The walk skips `dist`, `node_modules`,
`__pycache__` and the directories whose name starts with `.`. A BUILD file
that only sets `__defaults__` declares no target. The info diagnostic
`observe.pants_graph_missing` names the export command. An export that is not
a JSON array of targets, each with an `address`, is the warning
`observe.pants_graph_unreadable`, and the BUILD files answer instead.

## Reports

Run the tests with the reports on:

```bash
pants test --test-report --use-coverage --coverage-py-report=xml ::
```

`putnami observe` finds the JUnit files under `dist/test/reports` and the
`coverage.xml` under `dist/coverage/python` by their content, as any report.
Pants runs pytest from the root of its sandbox, so a test case's class name is
its file's path, such as `src.python.pricing.pricing_test`, and lands on the
component that holds `src/python/pricing/pricing_test.py`. The coverage report
names each file from the repository root, so the sandbox path in its
`<source>` does not matter.

## Lockfiles

Each lockfile the `[python.resolves]` table of `pants.toml` names is a
lockfile of ecosystem `pypi`:

```toml
[python.resolves]
python-default = "3rdparty/python/default.lock"
```

Pants writes it in the PEX JSON lock format, after a header of `//` comment
lines. `putnami observe` reads the project name, normalized as PyPI does, and
the version of each locked requirement of each locked resolve. A path that
starts with `%(buildroot)s/` is read from the repository root; a path outside
the repository is the warning `observe.pants_config_invalid`, and so is a
`pants.toml` that does not parse. Without a `[python.resolves]` table, no
Pants lockfile is read. JVM and Go resolves are not read.
