# @putnami/dotnet sample

Three .NET projects: `application`, an ASP.NET minimal API, and `tests`, an
xunit project with Coverlet, both reference `library`. `application` answers
`GET /` with the discounted total the library computes.

These projects carry the `e2e` tag, so the default gate skips them. To run
them, you need no .NET SDK: when none on `PATH` satisfies their
`global.json`, the first task installs one under the Putnami home.

```bash
putnami lint,test,build --tag e2e --projects /dotnet/samples/library,/dotnet/samples/tests,/dotnet/samples/application
putnami serve --projects /dotnet/samples/application --port 3456
putnami qualify /dotnet/samples/application --target local
putnami package --docker --docker-load --projects /dotnet/samples/application
```

`application/schema/http-routes.json` lists the routes `putnami qualify` requests.
