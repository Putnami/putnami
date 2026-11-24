# Changelog — integrations

## 0.5.0 — 2026-11-24

### Features

- feat(nx): a named target runs as the task the workspace maps it to (75e52eb)
- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)
- feat(integrations): one import rule for Nx, Turborepo, Rush and moon (bb0f2aa)
- feat(typescript): build compiles a library with tsc when its build script does (654f519)
- feat(typescript): serve, run and package a TypeScript workload on Node (b9bdeda)
- feat(typescript): lint runs a project's ESLint configuration (b24b609)
- feat(stacks,integrations): shared core helpers, one command surface, pinned SDKs and the Android SDK opt-in (de53a45)
- feat(typescript): test runs a project's Vitest or Jest suite with its JUnit and coverage reports (47fd2e3)
- feat(typescript): install with the repository's npm, pnpm or yarn and keep its lockfile (7de6c9a)
- feat(nx,turbo): a delegated workspace imports its projects one at a time (65d12fa)

### Fixes

- fix(cli): init --from completes an existing .gitignore and gives the right ESLint advice (819d61b)
- fix(cli): a file the sync writes keeps the repository's layout (b328ca3)
- fix(nx): an import keeps the dependsOn of the targets it moves (42dc19b)
- fix(typescript): an import writes biome.json, tsconfig.json and @types/bun only for a task that reads them (117a647)
- fix(pants): bound the sync's memory, replace the coverage list, resolve symlinked directories (3d26275)
- fix(cli,rush,turbo,moon): a task waits for the builds its tool runs before it (e909636)
- fix(moon): read the tasks of a moon 2.x export and fall back to the moon on PATH (a16f2a5)
- fix(turbo): a delegated task runs the repository's own turbo from node_modules/.bin (d71a23d)
- fix(import): a moved lint that only checked only checks (4bcac0b)
- fix(rush,moon): a fully imported project keeps the edges the sync writes (b687784)
- fix(workspace): an imported workspace loads when a package nests a member or the root has two manifests (30464a1)
- fix(nx,moon): the TypeScript extension names a project both claim (e1e577c)
- fix(cli,cli-model): a cycle among delegated projects loads and runs by component (5b06c41)
- fix(rush,bazel): a delegated lint or test runs its project alone, and every bazel call keeps one instrumentation filter (f4cfe30)
- fix(stacks): a moved TypeScript task does what its script did (b89a51d)
- fix(nx,moon): the sync reads the graph and tasks the tool resolves (0ce38c0)
- fix(typescript): an import keeps the repository's member list, lockfile and installer (c3bf9e4)
- fix(cli): declared_edge, --channel and the ESLint advice state what is true (0b3c10d)
- fix(pants,bazel,cli): keep target names out of project paths, test with bazel test, and gate only declared verbs (3525ba9)
- fix(toolchain): the Nx probe reads Nx's version, and a Go on PATH that meets the pin qualifies (9ee51d0)
- fix(cli): init --from and projects sync adopt only the members the monorepo tool lists (f465626)
- fix(cli,typescript): Putnami keeps the escapes and symbolic links of the files it writes (6f2858c)

### Other

- build(stacks): publish the stack extensions and tool integrations as archives (886d834)
- refactor: move the monorepo tool integrations to integrations/ (0198add)
