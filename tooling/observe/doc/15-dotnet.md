# How `putnami observe` reads a .NET repository

The .NET detector reads the solution and the project files of a C#, F# or
Visual Basic repository, and never runs `dotnet`, MSBuild or NuGet. The change
analyses use what it finds to name the changed and dependent projects, place
the tests and the coverage `dotnet test` wrote, and diff the NuGet lock files.

## Components

Each `.csproj`, `.fsproj` or `.vbproj` file is a component of ecosystem
`dotnet`:

| Project file | Source |
|---|---|
| listed by a `.sln` or `.slnx` file at the repository root | `workspace` |
| any other, found in the walk | `manifest` |

A component's id is the directory of its project file, and its name the file
name without its extension: `src/Pricing/Pricing.csproj` is the component
`src/Pricing`, named `Pricing`. When one directory holds several project
files, the first in name order names it, and the references of all of them
count. The walk skips `.git`, `bin`, `obj`, `node_modules`, `packages`,
`TestResults` and hidden directories, but a project a root solution lists is
read wherever it lies. A solution folder makes no component.

A solution that does not parse, or that lists a project file the repository
does not hold, gets `observe.dotnet_solution_invalid`, and a project file that
does not parse `observe.dotnet_project_invalid`: the project is still a
component, with no edge. Both are warnings.

## Edges

Each `<ProjectReference Include="...">` is an edge to the component whose
directory holds the project file it names. The path is relative to the
referencing project file's directory, with `\` or `/` separators, as
`dotnet add reference` writes it; an `Include` that lists several paths
separated by `;` gives an edge to each. A change to `src/Pricing` therefore marks
`src/Api`, `src/Worker` and `tests/Pricing.Tests` dependent when they
reference it:

```text
  components  1 changed, 3 dependent, 0 unaffected
    changed   src/Pricing (Pricing) [dotnet, workspace]
    dependent src/Api (Api) [dotnet, workspace], depends on src/Pricing
    dependent src/Worker (Worker) [dotnet, workspace], depends on src/Pricing
    dependent tests/Pricing.Tests (Pricing.Tests) [dotnet, workspace], depends on src/Pricing
```

MSBuild is not evaluated. A reference whose path holds a property, such as
`$(SolutionDir)`, a reference that leaves the repository or names no project
file of it, and a reference that `Directory.Build.props` or an import adds,
make no edge.

## Reports

Have `dotnet test` write a TRX report and Coverlet's Cobertura coverage:

```bash
dotnet test --logger trx --results-directory TestResults --collect:"XPlat Code Coverage"
```

`putnami observe` finds the `.trx` file by its `TestRun` root and the
`coverage.cobertura.xml` under `TestResults/` by its content; `--junit` names
TRX reports too. The JUnit report of `JunitXml.TestLogger`
(`--logger junit`) is read as well.

Neither report names a test file. A test case goes to the project of its test
assembly: TRX and `JunitXml.TestLogger` name the assembly file, such as
`Pricing.Tests.dll`, as the case's suite, and the project whose
`AssemblyName`, else whose project file name, is `Pricing.Tests` holds the
case. A namespace never names a project: tests in the namespace
`Pricing.Tests` of an assembly `Shop.Acceptance` go to the project of
`Shop.Acceptance`. A report that lies in a project's directory, as
`dotnet test` writes it without `--results-directory`, also places its
cases. Coverlet names each covered file relative to an absolute source root,
which `putnami observe` maps to the checkout even when the CI ran elsewhere.

## Lock files

With NuGet lock files on (`<RestorePackagesWithLockFile>true</RestorePackagesWithLockFile>`),
each changed `packages.lock.json` is a lockfile of ecosystem `nuget`: every
package each target framework resolves, direct, transitive or central, at its
resolved version. A `Project` entry is the repository's own project and is
left out. A package another project brings in transitively appears in the
lock file of each project that uses it, so its bump changes each of them.

Without lock files, a change to the versions of central package management,
each `<PackageVersion Include="..." Version="..." />` of a
`Directory.Packages.props`, is the dependency change. A version written as a
property, such as `$(XunitVersion)`, is not read. In a repository with lock
files, `Directory.Packages.props` is not read: the lock files carry the same
change, with the versions NuGet resolved.

A `nuget` package URL of an SBOM, such as `pkg:nuget/Newtonsoft.Json@13.0.3`,
matches a change by its name as written and its version.

## Limits

- Solutions below the root are not read: their projects are `manifest`
  components.
- `.sqlproj`, `.vcxproj`, `.esproj` and other project types are not
  components.
- MSBuild properties, conditions and imports are not evaluated. The nearest
  `Directory.Build.props` is read for one thing only: whether it makes its
  projects test projects, which `@putnami/dotnet` uses to run `dotnet test`
  in test projects only.
- The inner results of a data-driven TRX test are not cases: their parent
  stands for them.
