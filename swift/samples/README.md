# @putnami/swift sample

`application` is a Hummingbird server: `GET /` answers a greeting on the port
`PORT` names, and `GET /robots.txt` the file its target ships as a resource,
which the image holds in the resource bundle beside the executable.

The project carries the `e2e` tag, so the default gate skips it. Its
`.swift-version` states Swift 6.4: a task installs that release from swift.org
under the Putnami home when `PATH` has none, and `package` installs its Static
Linux SDK the first time. The Swift of Xcode cannot build with that SDK.

```bash
putnami install
putnami lint,build --tag e2e --projects swift-example-application
putnami serve --projects swift-example-application --port 3456
putnami qualify swift-example-application --target local
putnami package --docker --docker-load --projects swift-example-application
docker run --rm -p 8080:8080 swift-example-application:<tag>
```

`application/schema/http-routes.json` lists the routes `putnami qualify` requests.
