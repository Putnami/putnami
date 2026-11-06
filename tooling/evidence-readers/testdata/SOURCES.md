# Fixture sources

Every report under `junit/`, `lcov/`, `cobertura/`, `clover/`, `jacoco/` and `gocover/` is
the unmodified output of a real tool run, renamed only. Each `.golden.json` is
what the reader returns for it: regenerate them with `go test -update` and
review the diff. `malformed/` holds small hand-written reports, each listed in
`malformed/expectations.json` with the diagnostic code it must return.

Every stack tests the same code, kept under `sources/<stack>/`: three
functions, one never called and one with a half-taken branch; two passing
tests (one nested in a describe block or data provider), one failing and one
skipped.

| Stack | Tools | Command | Fixtures |
| --- | --- | --- | --- |
| Jest | jest 30.5.2, jest-junit 17.0.0, Node 24.22.2 in the `node:24` image | `JEST_JUNIT_ADD_FILE_ATTRIBUTE=true jest --ci --reporters=default --reporters=jest-junit --coverage --coverageReporters=lcov --coverageReporters=cobertura --coverageReporters=clover` | `junit/jest.xml`, `lcov/jest.info`, `cobertura/jest.xml`, `clover/jest.xml` |
| Vitest | vitest 5.0.5, @vitest/coverage-v8 5.0.5, same image | `vitest run --reporter=junit --outputFile=junit.xml --coverage.enabled --coverage.provider=v8 --coverage.reporter=lcov --coverage.reporter=cobertura --coverage.reporter=clover --coverage.reportOnFailure` | `junit/vitest.xml`, `lcov/vitest.info`, `cobertura/vitest.xml`, `clover/vitest.xml` |
| node:test with c8 | c8 12.0.0, Node 24.22.2, same image | `c8 --reporter=lcov --reporter=cobertura node --test --test-reporter=junit --test-reporter-destination=junit.xml` | `junit/node-test.xml`, `lcov/c8.info`, `cobertura/c8.xml` |
| PHPUnit | PHPUnit 11.5.57, PHP 8.3.36, PCOV 1.0.12, image from `sources/phpunit/Dockerfile` | `vendor/bin/phpunit --log-junit junit.xml --coverage-clover clover.xml --coverage-cobertura cobertura.xml` | `junit/phpunit.xml`, `clover/phpunit.xml`, `cobertura/phpunit.xml` |
| Maven | Maven 3.9.11, OpenJDK 21.0.12, maven-surefire-plugin 3.5.2, jacoco-maven-plugin 0.8.13, JUnit 5.11.4 | `mvn -B verify` | `jacoco/maven.xml` |
| Gradle with Kotlin | Gradle 8.14.3, Kotlin 2.0.21, the `jacoco` plugin with JaCoCo 0.8.13, OpenJDK 21.0.12, JUnit 5.11.4 | `gradle test`, which the build finalizes with `jacocoTestReport` | `jacoco/gradle-kotlin.xml` |
| Go | go 1.26.1, gotestsum 1.13.0 | `go test -coverprofile=set.coverprofile ./...`, `go test -covermode=atomic -coverpkg=./... -coverprofile=atomic.coverprofile ./...`, `gotestsum --junitfile junit.xml -- ./...` | `gocover/set.coverprofile`, `gocover/atomic.coverprofile`, `junit/gotestsum.xml` |

The Node and PHP projects ran mounted at `/app` in a container whose hostname
is `ci-runner`, so the absolute paths in the reports start with `/app`. The Go
module is `example.com/calc`; its `go.mod` is kept as `go.mod.txt` so that no
tool takes `sources/go` for a module. The Maven and Gradle projects ran in
`/ci/maven` and `/ci/gradle`; their `pom.xml`, `build.gradle.kts` and
`settings.gradle.kts` are kept with a `.txt` suffix for the same reason. Their
JaCoCo reports name no absolute path.

## Software bills of materials

`sbom/` holds the CycloneDX 1.5 and SPDX 2.3 documents osv-scanner 2.7.0
(`ghcr.io/google/osv-scanner`, osv-scalibr 0.5.3) wrote on 2026-11-02 for two
npm lockfiles, kept under `sources/sbom/`: the base locks lodash 4.17.20 and
minimist 1.2.5, the head locks lodash 4.17.21 and zod 3.23.8. The command, with
the lockfile mounted at `/src`:

```bash
osv-scanner scan source --all-packages --licenses --format cyclonedx-1-5 -L /src/package-lock.json
osv-scanner scan source --all-packages --licenses --format spdx-2-3 -L /src/package-lock.json
```

`base.cdx.json`, `head.cdx.json`, `base.spdx.json` and `head.spdx.json` are
unmodified. osv-scanner writes no vulnerability into SPDX, so
`head-advisories.spdx.json` is `head.spdx.json` with three `externalRefs`
added by hand: a `SECURITY` `advisory` reference on each package and a
`cpe23Type` one on lodash, the shape SPDX 2.3 gives a known vulnerability.
