# Fixture sources

Every report under `junit/`, `lcov/`, `cobertura/`, `clover/` and `gocover/` is
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
| Go | go 1.26.1, gotestsum 1.13.0 | `go test -coverprofile=set.out ./...`, `go test -covermode=atomic -coverpkg=./... -coverprofile=atomic.out ./...`, `gotestsum --junitfile junit.xml -- ./...` | `gocover/set.out`, `gocover/atomic.out`, `junit/gotestsum.xml` |

The Node and PHP projects ran mounted at `/app` in a container whose hostname
is `ci-runner`, so the absolute paths in the reports start with `/app`. The Go
module is `example.com/calc`; its `go.mod` is kept as `go.mod.txt` so that no
tool takes `sources/go` for a module.
