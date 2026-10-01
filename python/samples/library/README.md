# Experimental Python Library Example

This library demonstrates the explicitly opted-in `@putnami/python` extension.
It is an experiment, not a default Putnami path, production reference, or a
parity example for Go or TypeScript.

It exercises the [experimental workspace integration feature](../../extension/specs/experimental-workspace-integration.json),
not a separate framework or sample-parity feature.

## Features

- Library code with exported functions
- String utilities: `reverse`, `capitalize`, `is_palindrome`
- Math utilities: `clamp`, `fibonacci`, `is_prime`
- Unit tests with pytest
- Linting with Ruff

## Exported Functions

### String Utilities

| Function | Description | Example |
|----------|-------------|---------|
| `reverse(s)` | Reverses a string | `reverse("hello")` → `"olleh"` |
| `capitalize(s)` | Capitalizes first letter | `capitalize("hello")` → `"Hello"` |
| `is_palindrome(s)` | Checks if palindrome | `is_palindrome("racecar")` → `True` |

### Math Utilities

| Function | Description | Example |
|----------|-------------|---------|
| `clamp(val, min, max)` | Clamps value to range | `clamp(15, 0, 10)` → `10` |
| `fibonacci(n)` | First n Fibonacci numbers | `fibonacci(5)` → `[0, 1, 1, 2, 3]` |
| `is_prime(n)` | Checks if prime | `is_prime(17)` → `True` |

## Usage

```python
from pylib import reverse, capitalize, is_palindrome, fibonacci, is_prime

reverse("hello")  # "olleh"
capitalize("hello")  # "Hello"
is_palindrome("racecar")  # True
fibonacci(5)  # [0, 1, 1, 2, 3]
is_prime(17)  # True
```

## Commands

**Run tests:**

```bash
putnami test py_example_library
```

Expected result — all pytest tests pass.

**Lint code:**

```bash
putnami lint py_example_library
```

Expected result — no Ruff linting errors.

## Executable spec fixture

This sample is the Python conformance fixture of the executable spec gate:
`putnami.features.json` declares an acceptance check for the
`clamp-bounds` requirement of `specs/example-library-utilities.json`, and the
protecting tests bind themselves with the `putnami_proves` pytest marker —
plain metadata for bare `pytest`, recorded by the Putnami test adapter's
injected plugin. The project deliberately stays on the inherited workspace
`report` mode; `putnami specs verify py_example_library` replays the recorded
verdict. See `doc/adr/0001-executable-spec-fixture.md`.
