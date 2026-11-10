# @putnami/java samples

Three builds:

- `gradle/`: a Gradle build with its wrapper. `application` applies the
  `application` plugin and serves the total of a cart with the JDK's HTTP
  server, and depends on `library`.
- `maven/`: a Maven build on the Spring Boot parent. `application` is a
  Spring Boot web application that depends on `library`. Its
  `.mvn/wrapper/maven-wrapper.properties` names Maven 3.9.16, which runs in
  place of any other `mvn`.
- `gradle-toolchains/`: a Gradle build with no wrapper, which the pinned
  Gradle runs. Java 21 runs Gradle, a convention plugin of `buildSrc` gives
  `report` a Java 17 toolchain, and `legacy` asks for a Java 11 one; each
  test checks the Java version it runs on.

Each build's root is its components' stack root, and a project of its own.
These projects carry the `e2e` tag, so the default gate skips them. To run
them, you need no JDK, no Gradle and no Maven: the first task installs the
Eclipse Temurin JDKs and the Gradle or Maven each build needs.
[`java-stack.yml`](../../.github/workflows/java-stack.yml) runs them on a
machine that holds none, after `putnami install`.

```bash
putnami projects sync
putnami lint,test,build --tag e2e --projects /java/samples/gradle/library,/java/samples/gradle/application,/java/samples/maven/library,/java/samples/maven/application,/java/samples/gradle-toolchains/legacy,/java/samples/gradle-toolchains/report
putnami serve --projects /java/samples/gradle/application --port 3456
putnami run --projects /java/samples/gradle/application --args "1000 500"
putnami qualify /java/samples/maven/application --target local
putnami package --docker --docker-load --projects /java/samples/maven/application
```

`application/schema/http-routes.json` lists the routes `putnami qualify`
requests.
