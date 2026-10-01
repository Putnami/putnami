# Experimental Python Application Example

This FastAPI example depends on `py_example_library` and demonstrates the
explicitly opted-in `@putnami/python` extension. It is an experiment, not a
default Putnami path, production reference, or a parity example for Go or
TypeScript.

It exercises the [experimental workspace integration feature](../../extension/specs/experimental-workspace-integration.json),
not a separate framework or sample-parity feature.

## Features

- FastAPI HTTP server
- Depends on `py_example_library`
- String manipulation endpoints (reverse, capitalize, palindrome check)
- Math endpoints (Fibonacci, prime check, clamp)
- Tests with pytest and httpx TestClient
- Docker publishing via `putnami publish`

## Routes

| Method | Path                         | Description                          |
| ------ | ---------------------------- | ------------------------------------ |
| GET    | `/`                          | Health check                         |
| GET    | `/strings/reverse/{text}`    | Reverse a string                     |
| GET    | `/strings/capitalize/{text}` | Capitalize a string                  |
| GET    | `/strings/palindrome/{text}` | Check if palindrome                  |
| GET    | `/math/fibonacci/{n}`        | First n Fibonacci numbers (n: 0-100) |
| GET    | `/math/prime/{n}`            | Check if prime (n: 0-1000000)        |
| POST   | `/math/clamp`                | Clamp a value between bounds         |

## Dependencies

This project depends on `py_example_library`, which will have its workspace
synced first via the Python extension's `workspace-install` job. Validate the
dependency and runtime behavior in your own experiment before relying on it.

## Commands

**Run tests:**

```bash
putnami test py_example_application
```

Expected result — all pytest tests pass.

**Lint code:**

```bash
putnami lint py_example_application
```

Expected result — no linting errors.

**Serve the application:**

```bash
putnami serve py_example_application
```

## Try It

Once the server is running (on port 3930), open a new terminal:

**1. Health check:**

```bash
curl http://localhost:3930/
```

Expected response:

```json
{ "status": "ok", "service": "py_example_application" }
```

**2. Reverse a string:**

```bash
curl http://localhost:3930/strings/reverse/hello
```

Expected response:

```json
{ "input": "hello", "result": "olleh" }
```

**3. Capitalize a string:**

```bash
curl http://localhost:3930/strings/capitalize/hello
```

Expected response:

```json
{ "input": "hello", "result": "Hello" }
```

**4. Check if a string is a palindrome:**

```bash
curl http://localhost:3930/strings/palindrome/racecar
```

Expected response:

```json
{ "input": "racecar", "is_palindrome": true }
```

**5. Get Fibonacci sequence:**

```bash
curl http://localhost:3930/math/fibonacci/8
```

Expected response:

```json
{ "n": 8, "sequence": [0, 1, 1, 2, 3, 5, 8, 13] }
```

**6. Check if a number is prime:**

```bash
curl http://localhost:3930/math/prime/17
```

Expected response:

```json
{ "n": 17, "is_prime": true }
```

**7. Clamp a value:**

```bash
curl -X POST http://localhost:3930/math/clamp \
  -H "Content-Type: application/json" \
  -d '{"value": 15, "min_val": 0, "max_val": 10}'
```

Expected response:

```json
{ "value": 15.0, "min": 0.0, "max": 10.0, "result": 10.0 }
```
