# @putnami/java samples

Two builds, each an application that depends on a library:

- `gradle/`: a Gradle build with its wrapper. `application` applies the
  `application` plugin and serves the total of a cart with the JDK's HTTP
  server.
- `maven/`: a Maven build on the Spring Boot parent. `application` is a
  Spring Boot web application.

Each build's root is its components' stack root, and a project of its own.
These projects carry the `e2e` tag, so the default gate skips them. To run
them, you need no JDK and no Maven: `putnami install` pins an Eclipse Temurin
JDK, and the first Maven task installs Apache Maven.

```bash
putnami install
putnami projects sync
putnami lint,test,build --tag e2e --projects /java/samples/gradle/library,/java/samples/gradle/application,/java/samples/maven/library,/java/samples/maven/application
putnami serve --projects /java/samples/gradle/application --port 3456
putnami run --projects /java/samples/gradle/application --args "1000 500"
putnami qualify /java/samples/maven/application --target local
putnami package --docker --docker-load --projects /java/samples/maven/application
```

`application/schema/http-routes.json` lists the routes `putnami qualify`
requests.
