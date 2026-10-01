# Error Handling

Putnami provides structured error output and built-in reliability mechanisms for both interactive development and CI pipelines.

## Error output

In text mode (default), errors are printed to stderr:

```text
  putnami: unknown command 'deploy'
```

With `--output=jsonl`, job-level errors are reported as `diagnostic` and `result` events in the JSONL stream:

```jsonl
{"v": 1, "type": "diagnostic", "severity": "error", "message": "Cannot find name 'foo'", "code": "TS2304", "location": {"file": "src/app.ts", "line": 10, "column": 5}}
{"v": 1, "type": "result", "data": {"status": "FAILED", "error": {"message": "Compilation failed", "code": "BUILD_ERROR"}}}
```

## Common error types

| Type | Cause | What to check |
|------|-------|---------------|
| Validation error | Incorrect flags or arguments | `putnami <command> --help` |
| Configuration error | Invalid workspace or project config | Check `putnami.workspace.json` and `putnami.json` |
| Execution error | A job script failed | Re-run with `--verbose` or `--output=jsonl` |
| Command not found | Typo or missing extension | `putnami --help` to see available commands |
| No jobs matched | No projects matched the target expression | Check `--impacted` scope or use `--all` |

## Reliability flags

### Automatic retries

Use `--retry <N>` to retry failed jobs up to N times:

```bash
putnami build --all --retry 3
```

Each failed job is retried immediately, up to the configured number of attempts. The job only reports as failed after all attempts are exhausted.

### Continue on error

By default, Putnami stops on the first failure. Use `--continue-on-error` to proceed with independent jobs:

```bash
putnami test --all --continue-on-error
```

Dependent jobs are still skipped — if `pkg-a` fails, `pkg-b` that depends on it is skipped regardless.

## Diagnostics

### Verbose mode

Show job results, phase starts, and diagnostics:

```bash
putnami build --all --verbose
```

### Debug mode

Stream all job events in real-time — phases, logs, progress, metrics:

```bash
putnami build --all --debug
```

### JSONL output for CI

Re-run a failing job with `--output=jsonl` to get structured diagnostics with file, line, column, and message:

```bash
putnami test . --no-cache --output=jsonl
```

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | Generic error |
| `2` | Job failed |
| `3` | No jobs matched |
| `4` | Validation error (bad flags or arguments) |
| `130` | Interrupted (SIGINT / Ctrl+C) |
