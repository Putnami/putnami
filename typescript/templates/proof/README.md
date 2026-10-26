# @putnami/typescript-templates-proof

Proves that every TypeScript template in this directory renders into a project that passes the Biome check `putnami lint` runs, type-checks and passes its own tests against this repository's framework sources. The lint step runs Biome with the TypeScript extension's `config/biome.json`, as a workspace made by `putnami install` does, and fails on a warning too. When Biome is not installed, the proof fails and names `putnami install`.

Each template declares its own feature in its `putnami.features.json`. Each step of the proof observes one check of that feature through `specTest`. When a session runs the proof's tests, the spec gate joins those observations to the template's requirements. This project owns no feature: its `featureAuthority` names `tooling/project-scaffolding`, the parent of every template feature.

## Commands

```bash
putnami test @putnami/typescript-templates-proof  # render and run every template
```

## Documentation

`test/templates.test.ts` describes each step. The templates themselves are documented by the README each one renders: [typescript-library](../typescript-library/README.md.template), [typescript-server](../typescript-server/README.md.template) and [typescript-web](../typescript-web/README.md.template).
