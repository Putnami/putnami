# Go App with Library Example

A Go application example that depends on the `library` module, demonstrating the `@putnami/go` plugin with workspace dependencies.

## Features

- Depends on `go.putnami.dev/examples/library`
- Demonstrates cross-compilation
- Includes tests
- Can be served as a web server

## Dependencies

This project depends on `go.putnami.dev/examples/library`, which will be built first due to the `^build` dependency in the plugin configuration.

## Commands

Run these commands from the workspace root so they use the checked-in wrapper
and the sample's exact project identity.

**Build the application:**

```bash
./putnamiw build --projects go.putnami.dev/examples/application
```

Expected result — compiles the binary successfully.

**Cross-compile for different platforms:**

```bash
./putnamiw build --projects go.putnami.dev/examples/application --target linux/amd64
./putnamiw build --projects go.putnami.dev/examples/application --target darwin/arm64
```

Expected result — produces platform-specific binaries.

**Run tests:**

```bash
./putnamiw test --projects go.putnami.dev/examples/application --enforce-coverage
```

Expected result — all tests pass with coverage report.

**Lint code:**

```bash
./putnamiw lint --projects go.putnami.dev/examples/application
```

Expected result — no linting errors.

**Serve the application:**

```bash
./putnamiw serve --projects go.putnami.dev/examples/application
```

## Try It

Once the server is running, open a new terminal:

```bash
curl http://localhost:3920/
```

Expected response:

```
Go App with Library Example
============
Library functions:
Reverse('hello') = olleh
Capitalize('world') = World
IsPalindrome('racecar') = true
```

Together with the library sample, this project is executable evidence for the
Go workspace build, test, lint, serve, dependency, and cross-compilation paths.
