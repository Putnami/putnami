# Configuration & Caching

TypeScript jobs are cache-aware. Cache inputs are declared per phase so `--impacted` and repeated local runs can skip work that has not changed.

## Job dependencies

| Job | Depends on | Cache behavior |
|-----|------------|----------------|
| `generate` | `^generate` | Cached from source, package metadata, and generation inputs |
| `build` | `generate`, `^build` | Cached from source, config, package metadata, and docs/assets |
| `test` | `generate` | Cached from source, tests, and package metadata |
| `lint` | none | Cached from source, Biome config, and package metadata |
| `serve` | `build --fast` | Never cached |
| `publish` | `build --transpile --types`, `^publish` | Uses build outputs |

## Common cache inputs

Build and test cache keys include files such as:

- `src/**/*.ts`
- `src/**/*.tsx`
- `test/**/*.ts`
- `test/**/*.tsx`
- `package.json`
- `tsconfig.json`
- `tsconfig.app.json`
- `tsconfig.lib.json`
- `biome.json`
- `doc/**/*`

## Project options

Use project configuration for behavior that should be reproducible across local, CI, and preview runs:

- generated asset paths
- compile targets
- publish channels
- serve port choices
- coverage thresholds
- Biome config paths

Run the final branch check through Putnami, not a language-specific tool:

```bash
putnami lint,test,build --impacted
```
