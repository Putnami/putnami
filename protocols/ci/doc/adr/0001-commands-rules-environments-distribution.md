# ADR 0001 — `putnami.ci.json`: commands, rules, environments, distribution

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/ci` (`protocols/ci`)

## Context

A repository must tell its runner which commands every push runs, which
channels a trigger publishes, which environments follow which channel, and
what it declares to its release-set provider: namespace, channels, visibility,
registries and their mirrors. Deploy tied to a branch rule cannot express a
stable channel a human moves, an environment that follows a channel, a
progressive rollout, or a preview per pull request. The workspace config must
not carry these Cloud concerns, because extensions work without Cloud.

## Decision

1. **`commands`** is an ordered list of command names, run as one invocation
   `putnami <c1,c2,…> --impacted <flags>`. An entry may be an object
   `{ "name", "failOnError": false }`: the command runs, its errors are reported
   as warnings, the run passes. `flags` is a list appended to the invocation;
   each command takes the flags it declares.
2. **`runner`** carries what the runner needs to configure itself and nothing
   else. Its content is open and owned by the runner implementation.
3. **`rules`** are ordered, first match wins. A rule selects exactly one
   trigger, `branches`, `tags`, or `pullRequests: true`, and lists in
   `publish` the channels a publish advances, in `--impacted` mode. A tag
   publishes by convention without a rule: implicit `--all` for the tag's line
   and an immutable channel named after the tag; a `tags` rule with
   `publish: false` opts out. A branch or a pull request without a matching
   rule only runs `commands`. Rules carry no deploy.
4. **`envs`** declare environments. An environment follows a `channel`; when
   the channel moves, the environment synchronizes its workloads on the named
   set under its `constraints`, `approval: manual` being the one every
   implementation supports. `rollout` sets how it advances, `variants` route
   parallel channels beside the main one, and `workloads` lists ordered
   selection rules, first match, each able to override `channel`, `rollout`,
   or `constraints`. A workload no rule selects is not part of the
   environment. An environment without `channel` only reacts to the command a
   person or an agent types.
5. **`distribution`** carries what the repository declares to its release-set
   provider: `namespace`; the visibility chain (`visibility` for the
   repository, `registries.<eco>.visibility`, `channels.<c>.visibility`,
   `versions.stable` and `versions.prerelease`, `members[]` selection rules);
   `channels.<c>.protected: true` for a channel no rule may target and no CI
   principal may move; `registries.<eco>.mirror.to`, the external target
   `public` members are copied to; and the opt-ins `memberAttribution` and
   `memberSourceTree`
   ([`protocols/distribution` ADR 0005](../../../distribution/doc/adr/0005-member-attribution-project-and-kind.md)).
6. **`validate` refuses** a rule that targets a protected channel, a channel
   name outside the portable alphabet, and a `distribution` or `envs` section
   in a workspace without a release-set provider.
7. **The document is JSON**, version 3, closed root. A new optional member is
   additive: a document that does not declare it keeps its canonical bytes and
   SHA-256 digest. Consumers pin a module release that owns a member before
   reading it.

## Document shape

```jsonc
{
  "version": 3,
  "commands": ["lint", "test", "build", "validate", { "name": "audit", "failOnError": false }],
  "flags": ["--enforce-coverage"],
  "distribution": {
    "namespace": "putnami",
    "visibility": "internal",
    "registries": { "npm": { "mirror": { "to": "https://registry.npmjs.org" } } },
    "channels": { "latest": { "visibility": "public", "protected": true } },
    "versions": { "stable": "public", "prerelease": "internal" },
    "members": [{ "select": "tag:public-lib", "visibility": "public" }]
  },
  "rules": [
    { "branches": "main", "publish": ["canary"] },
    { "tags": "wip/*", "publish": false },
    { "pullRequests": true, "publish": ["pr-{number}"] }
  ],
  "envs": {
    "staging": { "channel": "canary" },
    "prod": {
      "channel": "latest",
      "constraints": { "approval": "manual" },
      "workloads": [
        { "select": "tag:api", "rollout": { "strategy": "progressive", "steps": [10, 100], "advance": "manual" } },
        { "select": "group:experimental", "channel": "canary" }
      ]
    }
  }
}
```

`select` values use the CLI selection vocabulary: `tag:<tag>`, `group:<group>`,
`scope:<path>`, or a project id.

## Consequences

- The runner builds the `putnami` invocation from `commands` and `flags`.
- The runner never deploys from a branch rule. It reacts to a channel move by
  synchronizing the environments that follow that channel, honoring their
  constraints; `putnami deploy --env <name>` from a laptop does the same.
- The CLI reads `envs` for `deploy --env` and `distribution` to build the
  `release` request; it computes no visibility itself.
- `putnami ci explain` reports the commands, the matching rule, and the
  environments that follow each channel the rule advances. It resolves no
  remote decision and labels each one unresolved.
- The agent guidance generator derives its gate line from `commands`.
