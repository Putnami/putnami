# Run a .NET repository with Putnami

`@putnami/dotnet` turns each C#, F# and Visual Basic project of a .NET
repository into a Putnami project and runs its lint, test and build with the
`dotnet` commands the team already uses. It serves and runs an application,
and packages and publishes its OCI image. It reads the repository the way
`putnami observe` does, and installs the SDK `global.json` states when the
machine has none.

## Adopt it

1. At the root of the repository, run:

   ```bash
   putnami init --extension dotnet
   ```

   It writes `putnami.workspace.json` with `@putnami/dotnet` declared, installs
   the extension, and runs `putnami projects sync`, because the repository
   holds a `.csproj`, `.fsproj` or `.vbproj`. A repository of several languages takes a list, such as
   `--extension dotnet,typescript`, and `putnami init` without `--extension`
   proposes the languages it finds. In a workspace that already exists,
   declare the extension in `putnami.workspace.json` and run
   `putnami projects sync`:

   ```json
   { "extensions": ["@putnami/dotnet"] }
   ```

2. Each directory that holds a `.csproj`,
   `.fsproj` or `.vbproj` joins the workspace's `includes`. No `putnami.json`
   is needed: the extension names the project by its project file, adds an
   edge to each project it references with a `ProjectReference`, and tags it
   `dotnet`.
3. Run `putnami lint,test,build`.

## What each task runs

Each task runs in the project's directory, with no shell. `{manifest}` is the
project file, named in full so a folder that also holds a solution builds the
project, not the solution.

| Task | Step |
|---|---|
| `lint` | `dotnet format {manifest} --verify-no-changes` |
| `test` | `dotnet test {manifest} --logger trx --results-directory {output} --collect "XPlat Code Coverage"` |
| `build` | `dotnet build {manifest}` |

Only a test project runs `test`; any other project passes its test task with
nothing run. A project is a test project when, outside any MSBuild condition,
`Choose` branch or target, its project file states
`<IsTestProject>true</IsTestProject>`, references the `Microsoft.NET.Test.Sdk`
package or uses the `MSTest.Sdk` SDK, and states no
`<IsTestProject>false</IsTestProject>`; or, when its project file states none
of those, when the nearest `Directory.Build.props` states one of the first
three and no `false`. The test task decides it again when it runs, so an edit
of the project file or of a `Directory.Build.props`, which its key covers,
takes effect on the next run. The projects `dotnet new xunit`, `nunit` and `mstest` write are test
projects. A test project the rule misses, because a condition or an import
marks it, states `<IsTestProject>true</IsTestProject>` in its project file;
a helper project that a `Directory.Build.props` marks by mistake states
`false`.

The test task reads every TRX report in its output directory and the
`coverage.cobertura.xml` that Coverlet's collector writes one directory below
it into the test and coverage summary, even when `dotnet test` exits
non-zero. The `coverlet.collector` package that `dotnet new` adds to a test
project writes it. A failed case fails the task with `stacks.test_failed`,
which names it; a step that exits non-zero with no failed case, or a missing
TRX report, fails it too. A test project with no Coverlet collector writes no
Cobertura report: the task passes with the warning
`stacks.coverage_unavailable` and no coverage, and `--enforce-coverage` fails
it. The output directory is emptied before
the step.

`lint` also checks every relative link and anchor of the project's
`README.md` files and `doc/` trees with the documentation link check every
language extension shares; `--docs-links=false` turns it off.

A task of a project waits for the same task of the projects it references.
Every project has the workspace root for stack root, because MSBuild reads
`global.json`, `Directory.Build.props` and `NuGet.config` up to it and a
build writes the `obj/` of each project it references: the .NET tasks of a
workspace run one at a time.

## Restore and upgrade the packages

`dotnet build`, `dotnet test` and `dotnet format` restore their project
themselves. The workspace commands restore every project ahead of them:

| Job | What it runs |
|---|---|
| `workspace-install`, run by `putnami install` | `dotnet restore <project file>` for every project of the workspace that `disable.tags` keeps, with `--locked-mode` for a project that holds a `packages.lock.json`, so the install keeps its locked versions. `--force` adds `--force`. |
| `workspace-fetch`, run by `putnami install` | The same restore, which downloads the packages into the NuGet global packages folder. A hosted run calls it with the network before the tasks, which then restore from that folder. |
| `deps-upgrade`, run by `putnami upgrade` | `dotnet package update --project <project file>` for every project, which moves each package reference to the newest version its sources hold and restores the project, its `packages.lock.json` included. It needs the .NET 10 SDK and the network, and fails with `dotnet.package_update_failed` otherwise. |

A restore that fails fails the command with `dotnet.restore_failed`. When the
machine has no SDK `global.json` accepts, the command installs one as a task
does; offline, it leaves the workspace out with a `stacks.toolchain_missing`
warning.

On a hosted run, the engine hands `workspace-fetch` a read credential on an
inherited descriptor. The job reads that descriptor and closes it before it
starts any process, the SDK check included, so no `dotnet` inherits it or
finds the credential in its environment. The job then uses nothing the
descriptor held: the credential's hosts are the Putnami registries, which
serve no NuGet package, so no NuGet source can use it. `dotnet restore`
reaches nuget.org, which needs no credential to download, and the sources a
`NuGet.config` names with the credentials that file or a NuGet credential
provider holds on the machine. A descriptor the job cannot read fails it with
`dotnet.restore_failed` before any process starts.

## The SDK check

Before its step, each task runs `dotnet --version` in the project's
directory, which prints the SDK the build will use, and checks it against the
`sdk.version` of the nearest `global.json`, in the project's directory, then
at the workspace root, under its `sdk.rollForward` policy. A prerelease
orders below its release:

| `rollForward` | The SDK matches when it is |
|---|---|
| `disable` | the same version |
| `patch`, `latestPatch`, or none | in the same feature band, with a patch at least the expected one: `8.0.400` accepts 8.0.404, not 8.0.500 |
| `feature`, `latestFeature` | in the same major and minor release, at least the expected version |
| `minor`, `latestMinor` | in the same major release, at least the expected version |
| `major`, `latestMajor` | at least the expected version |

| Case | Result |
|---|---|
| The SDK matches | The task runs |
| It does not | The task installs one, as the next section says; offline, it fails with `stacks.toolchain_mismatch`, and `toolchain.check: warn` runs it anyway |
| `dotnet --version` fails, because no installed SDK satisfies `global.json` | The task fails with `stacks.toolchain_mismatch`, which lists the SDKs `dotnet --list-sdks` names; `warn` lets the step run, and `dotnet` refuses it |
| `dotnet` is not on `PATH`, or no SDK matches | The task installs the SDK, as the next section says, and runs with it; offline, it fails with `stacks.toolchain_missing` or `stacks.toolchain_mismatch` |
| No `global.json` states a version | The warning `stacks.toolchain_unpinned`: the cache key carries no SDK version |

The cache key carries the version `global.json` states, not the machine's:
two machines whose SDKs the policy accepts share entries.

## Where the SDK comes from

The SDK on `PATH` comes first. When it is missing, or no SDK on `PATH`
satisfies `global.json`, the task runs its steps with one under
`<PutnamiHome>/tools/dotnet/sdk-<version>_<platform>`, `DOTNET_ROOT` set:

1. an SDK an earlier task installed there for the same platform, the one the
   rules below pick among them; this needs no network;
2. else it reads Microsoft's release metadata,
   `builds.dotnet.microsoft.com/dotnet/release-metadata`, for the channels the
   roll-forward policy reaches, and picks the SDK the .NET host would: the
   stated one for `disable`, `patch` or no policy (for `patch` or none, the
   latest of its feature band when the stated one is not published), the latest patch of
   the stated feature band, else of the next one, for `feature`, `minor` and
   `major`, the highest for the `latest*` policies, and with no `global.json`
   the latest SDK of the 10.0 channel. A prerelease is picked only when
   `global.json` states one;
3. it installs, beside the SDK, the latest ASP.NET Core runtime, which holds
   the .NET runtime, of every channel from 8.0 up to the SDK's, whatever its
   support phase, since an SDK carries only its own runtime and a project may
   target an older one. Each archive is checked
   against the SHA-512 the metadata states.

An offline run (`PUTNAMI_OFFLINE_DEPENDENCIES=1`) installs nothing, and
reuses none either: it checks the SDK on `PATH` alone. Linux, including a
musl system (one whose `/bin/sh` runs on musl), macOS and Windows on x64 and arm64 are supported. An
installation holds the machine's lock for its whole download, and an SDK a
later one supersedes stays until it is removed by hand.

## Run an application

A project is an application when its project file uses the
`Microsoft.NET.Sdk.Web` or `Microsoft.NET.Sdk.Worker` SDK or states an `Exe`
or `WinExe` `OutputType`, outside any condition, and is no test project. Any
other project is a library, which has no `serve`, `run`, `package` or
`publish`.

| Command | Runs |
|---|---|
| `putnami serve` | `dotnet build <project file>` under the stack-root lock, then `dotnet run --no-build --project <project file> --no-launch-profile`, with `ASPNETCORE_URLS=http://0.0.0.0:<port>` and `PORT=<port>`; `--port` defaults to 3000, and 0 picks a free port. The application is ready once its port accepts connections. |
| `putnami run` | `dotnet build`, then `dotnet run --no-build --project <project file> --no-launch-profile -- <args>`, with the words of `--args`, and exits with the application's exit code. |

`putnami qualify <project> --target local` serves an application and
requests the routes its `schema/http-routes.json` lists.

## Package and publish an image

`putnami package --docker` assembles the image of an application whose
`putnami.json` sets `options.package.docker`, with no Docker daemon:

1. `dotnet publish <project file> --configuration Release --framework
   <target framework> --runtime linux-x64 --self-contained false
   -p:UseAppHost=false`, `linux-arm64` with `--platform linux/arm64`;
2. a layer of the published files of its dependencies, then a layer of its
   own (`<AssemblyName>.*`, `appsettings*`, `web.config`, `wwwroot/`), under
   `/app`;
3. on the official `mcr.microsoft.com/dotnet/aspnet` image of the target
   framework when the published `runtimeconfig.json` needs ASP.NET Core, a
   framework reference included, `runtime` otherwise, pinned by digest
   for .NET 8.0, 9.0 and 10.0. `--docker-base-image` names another base,
   which must carry its digest, or the task fails with
   `dotnet.base_not_pinned`.

The publish runs under the stack-root lock, since it restores and builds the
referenced projects in place. The image leaves out the `putnami.json`,
`global.json` and `schema/http-routes.json` the SDK copies into its output.
It runs `dotnet /app/<AssemblyName>.dll` as the non-root `app` user in
`/app`; an ASP.NET Core application listens on 8080 (`ASPNETCORE_HTTP_PORTS` and
`PORT`). A project that targets several frameworks names one with
`--framework`. `--docker-load` also loads the image into the local Docker
daemon, and `--docker-tag` adds a tag. The image name is the project name in
lowercase.

`putnami publish --docker` pushes it through the shared publisher, to the
registry `--docker-registry` names, else the `registries.oci.publish` registry
of the project, else of the workspace. An
application whose `putnami.json` sets `options.publish.docker`, or lists
`docker` in `publish`, is a release-set member of the OCI ecosystem.

## What reaches the cache key

A task's key covers the project's files its task reads; every
`Directory.Build.props`, `Directory.Build.targets`, `.editorconfig`,
`Directory.Build.rsp`, `*.globalconfig`, `Directory.Packages.props`,
`NuGet.config` in its three spellings and `global.json` of the workspace,
since MSBuild and the analyzers read them in each directory up to the root; the project's `putnami.json`;
the project file, the tasks it runs and the expected SDK version; and the key
of the same task of each project it references. An edit of one of those
workspace files reruns every .NET task, unless it lies under a `bin`, `obj`
or `TestResults` directory, where a build copies them. Under the project, no
file in a
`bin`, `obj`, `TestResults`, `node_modules`, `.gen` (Putnami's own), `.vs`,
`.idea`, `.vscode`, `.git` or `.putnami` directory is an input, so neither a
build nor an IDE changes the next run's key.

| Input set | Patterns |
|---|---|
| `sources` | `**/*.cs`, `**/*.fs`, `**/*.fsi`, `**/*.vb`, `**/*.razor`, `**/*.cshtml`, `**/*.xaml`, `**/*.resx` |
| `analyzers` | `**/.editorconfig`, `**/*.globalconfig`, `BannedSymbols.txt`, `PublicAPI.Shipped.txt`, `PublicAPI.Unshipped.txt`, `stylecop.json` |
| `projects` | `*.csproj`, `*.fsproj`, `*.vbproj`, `*.user`, `Directory.Build.props`, `Directory.Build.targets`, and the workspace files above |
| `packages` | `packages.lock.json`, and every `Directory.Packages.props` of the workspace |
| `project` | `**`: every file of the project, such as a `.proto`, test data or a snapshot |
| `notTestSettings` | `!**/*.runsettings`, `!**/xunit.runner.json`, `!**/testconfig.json`: exclusions |
| `buildOutputs` | `!**/bin/**`, `!**/obj/**`, `!**/TestResults/**`, `!**/node_modules/**`, `!**/.gen/**`, `!**/.vs/**`, `!**/.idea/**`, `!**/.vscode/**`, `!**/.DS_Store`, `!**/*.DotSettings.user`: exclusions |

`lint` reads `sources`, `analyzers`, `projects` and `packages`; `test` reads
`project`, `projects` and `packages`; `build` reads the same as `test` but
the test settings, through `notTestSettings`. A change to a test setting
reruns no lint and no build; a change to a `.proto` or a test data file
reruns build and test, not lint. Each task reads `buildOutputs`, and an
exclusion applies to every other set of its task.

## Override the profile

A project overrides any value of the profile in its own `putnami.json`, under
`options["@putnami/dotnet"]`. An object merges; any other value, such as a
list of steps or patterns, replaces the profile's whole. The overrides a team
uses most:

```json
{
  "options": {
    "@putnami/dotnet": {
      "tasks": {
        "build": { "steps": [["dotnet", "build", "{manifest}", "--configuration", "Release", "-warnaserror"]] },
        "lint": { "steps": [["dotnet", "format", "whitespace", "{manifest}", "--verify-no-changes"]] }
      },
      "inputs": { "sources": ["**/*.cs", "**/*.cshtml", "Templates/**"] },
      "toolchain": { "check": "warn" }
    }
  }
}
```

- **A JUnit logger.** A test project that references `JunitXml.TestLogger`
  can report JUnit instead of TRX: replace the test step's
  `--logger trx` with `--logger "junit;LogFilePath={output}/junit.xml"`, and
  `tasks.test.reports` with `{"junit": ["{output}/junit.xml"], "coverage": [{"format": "cobertura", "paths": ["{output}/*/coverage.cobertura.xml"]}]}`.
- **No coverage.** A test project without `coverlet.collector` runs and warns
  with `stacks.coverage_unavailable`. To silence the warning, it drops
  `--collect "XPlat Code Coverage"` from its step and sets
  `tasks.test.reports` to `{"trx": ["{output}/*.trx"]}`.
- **Microsoft.Testing.Platform.** A test project that runs on it replaces the
  test step with the command line its runner takes, writing a TRX report into
  `{output}`.

A steps override keeps the profile's reports: a test step must still write
them, or the override also replaces `tasks.test.reports`. An override fails
the task with `stacks.project_config_invalid` when it names an unknown member,
changes `id` or `markers`, or adds a workspace-root input the extension does
not declare. The design of the stack core is in
[its design note](../../../tooling/stacks/doc/01-design.md).

## Limits

- MSBuild is not evaluated: a reference or an assembly name that holds a
  property, an import, and a test marker inside a condition are not read.
- A `global.json` between a project and the workspace root reaches the key,
  but the SDK check reads the project's and the root's only.
- `putnami <task> --impacted` maps a change to a workspace file above to the
  projects only when the file is at the root; a run without `--impacted`
  reruns what the change affects.
- Each test case and coverage line is not yet mapped to its project.
- `build` and `test` read every file under the project, those of another
  project nested in its directory included: a .NET project at the
  workspace root reruns when a nested project changes, such as the
  `ClientApp/` of the ASP.NET single-page templates, and resolving its inputs
  while that project's build rewrites its output can fail the task. Narrow
  its `project` input set in its `putnami.json`, such as
  `"project": ["**", "!ClientApp/**"]`.
- No NuGet publish: a package reaches NuGet through `dotnet pack` and
  `dotnet nuget push`.
- An image is framework-dependent: no self-contained, trimmed or
  ahead-of-time compiled image, and no other publish flag.
- `serve` and `run` take no `--framework`: a project that targets several
  frameworks fails in `dotnet run`. A worker application is never announced
  ready, since it opens no port.
- A stopped `serve` sends `dotnet run` SIGTERM and kills it 3 seconds later;
  an application that takes longer to stop may outlive it.
