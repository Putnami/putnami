# Test & Lint

Python test and lint jobs use native Python tools through the Putnami runner.

## Test

Tests run through pytest:

```bash
putnami test .
putnami test . --log
```

Behavior:

- discovers `**/test_*.py`
- returns `SKIP` when no tests are found
- executes through `uv run pytest`

Options:

- `--log`
- `--update-snapshots`
- `--test <filter>` or `-t`

## Lint

Lint uses Ruff:

```bash
putnami lint .
putnami lint . --fix false
```

The default path applies fixes when possible. Use `--fix false` for read-only checks.
