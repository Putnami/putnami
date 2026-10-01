# Sample Shell Extension

A reference implementation of a Putnami extension using shell scripts.

This extension demonstrates how to build Putnami extensions in **any language** without requiring npm, TypeScript, or Bun. It provides `build` and `test` jobs for Makefile-based projects.

## Structure

```
shell-extension/
├── putnami.extension.json    # Extension manifest with embedded flag definitions
├── bin/
│   ├── putnami-jsonl.sh      # Reusable JSONL helper library (source this)
│   ├── build                 # Build job (shell script)
│   └── test                  # Test job (shell script)
└── README.md
```

## How It Works

1. **Manifest** (`putnami.extension.json`) declares jobs with embedded `flags` — the SDK generates CLI commands from these definitions instead of requiring TypeScript modules.

2. **Executables** (`bin/build`, `bin/test`) receive:
   - `--putnamiContext <file>` — JSON file with workspace, project, and parameter context
   - `--output jsonl` — output mode flag
   - Job-specific flags (e.g., `--target release`, `--verbose`)

3. **JSONL events** are emitted on stdout following the protocol specification. The `putnami-jsonl.sh` helper library provides emit functions for all event types.

## Registration

Add this extension to your workspace's `.putnamirc.json`:

```json
{
  "extensions": [
    "./tooling/samples/shell-extension"
  ]
}
```

## Protocol Reference

See the [Extension SDK](../../extension-sdk/README.md) for the shared protocol and helper libraries, including:

- Context file schema
- JSONL event types and formats
- Exit code conventions
- Examples in Go, Python, and Shell
