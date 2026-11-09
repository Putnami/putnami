# @putnami/swift sample

`application` is a Hummingbird server: `GET /` answers a greeting on the port
`PORT` names.

The project carries the `e2e` tag, so the default gate skips it. To run it,
you need a Swift 6.3 or 6.4 toolchain from swift.org on `PATH`. `package`
installs the Static Linux SDK of that Swift the first time.

```bash
putnami install
putnami lint,build --tag e2e --projects swift-example-application
putnami serve --projects swift-example-application --port 3456
putnami qualify swift-example-application --target local
putnami package --docker --docker-load --projects swift-example-application
docker run --rm -p 8080:8080 swift-example-application:<tag>
```

`application/schema/http-routes.json` lists the routes `putnami qualify` requests.
