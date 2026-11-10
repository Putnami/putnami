# @putnami/dart sample

`application` is a `dart:io` server: `GET /` answers a greeting on the port
`PORT` names, 8080 without one, and it stops on `SIGINT` or `SIGTERM`. Its
only dependency is `test`, for its tests.

The project carries the `e2e` tag, so the default gate skips it, and
`workspace-install` leaves it out: its `pubspec.lock` is committed. Its
`pubspec.yaml` states `environment.sdk: ^3.8.0`, the first Dart that compiles
an executable for another architecture: a task installs the newest stable
release in it under the Putnami home when `PATH` has no `dart`.

```bash
putnami lint,test,build --tag e2e --projects dart-example-application
putnami serve --tag e2e --projects dart-example-application --port 3456
putnami qualify dart-example-application --target local
putnami package --tag e2e --projects dart-example-application --docker-load
docker run --rm -p 8080:8080 dart-example-application:<tag>
```

`--platform linux/arm64` packages the arm64 image. `application/schema/http-routes.json`
lists the routes `putnami qualify` requests.
