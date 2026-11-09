# @putnami/dotnet

`@putnami/dotnet` runs the C#, F# and Visual Basic projects of a .NET
repository as Putnami projects: `putnami lint`, `putnami test` and
`putnami build` run each project's `dotnet` steps with Putnami's runner and
cache, without changing the build; `install` and `upgrade` restore and
update the NuGet packages; `serve`, `run`, `package` and `publish` run an
application and ship its OCI image, and a missing SDK is installed.

It is a stack extension: a manifest, the .NET stack profile (`profile.json`),
the SDK roll-forward rule and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The projects and
their references come from the .NET detector of `putnami observe`.

See [the reference](doc/01-dotnet.md).
