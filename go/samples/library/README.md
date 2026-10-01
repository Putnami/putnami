# Go Library Basics Example

A Go library example demonstrating the `@putnami/go` plugin.

## Features

- Library code with exported functions
- Unit tests with coverage
- Linting with golangci-lint and staticcheck

## Exported Functions

| Function | Signature | Description |
|----------|-----------|-------------|
| `Reverse` | `Reverse(s string) string` | Reverses a string |
| `Capitalize` | `Capitalize(s string) string` | Capitalizes the first letter |
| `IsPalindrome` | `IsPalindrome(s string) bool` | Checks if string is a palindrome (case-insensitive) |

## Usage

```go
import golib "go.putnami.dev/examples/library"

golib.Reverse("hello")         // "olleh"
golib.Capitalize("hello")      // "Hello"
golib.IsPalindrome("racecar")  // true
golib.IsPalindrome("hello")    // false
```

## Commands

Run these commands from the workspace root so they use the checked-in wrapper
and the sample's exact project identity.

**Build the library:**

```bash
./putnamiw build --projects go.putnami.dev/examples/library
```

Expected result — compiles successfully with no errors.

**Run tests:**

```bash
./putnamiw test --projects go.putnami.dev/examples/library --enforce-coverage
```

Expected result — all tests pass with coverage report.

**Lint code:**

```bash
./putnamiw lint --projects go.putnami.dev/examples/library
```

Expected result — no linting errors.

This project is executable evidence for a publishable Go library with exported
APIs, unit tests, and workspace dependency consumption.
