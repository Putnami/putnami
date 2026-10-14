# Repository policy

A repository states its contributor conventions once, in the
`options["@putnami/contributor"]` block of `putnami.workspace.json`. Skills and
helpers read it from there; the shipped reference
(`.agents/skills/build/references/policy.md`) lists every member, its
default and which workflow reads it.

The block holds conventions only:

- **Not credentials.** Providers resolve their own.
- **Not provider settings.** A repository name, a state-to-label mapping, label
  inheritance or self-assignment belong to the provider binding in
  `options.collaboration`.
- **Not host policy.** Permissions, hooks and model access stay in the host's
  configuration (`.claude/settings.json`, `.codex/config.toml`).

A workspace without the block gets the defaults: claimed and delivered state
`in_progress`, the shipped worker profiles, no language rule,
no hosted gate, proposals ready at finalization, no body limit beyond the
contract's, no task reference line, no integration.

## Reference: this repository's configuration

Putnami binds tasks and proposals to the GitHub provider and sets the
conventions its maintainers use. It is a reference, not a default for other
repositories.

```json
{
  "options": {
    "collaboration": {
      "tasks": {
        "provider": "@putnami/github-collaboration",
        "version": 1,
        "require": ["assign"],
        "settings": {
          "repository": "Putnami/putnami",
          "states": {
            "open": ["status/confirmed", "status/audit-finding"],
            "in_progress": ["status/in-progress"],
            "blocked": ["status/needs-review", "status/needs-design"]
          },
          "stateLabelPrefix": "status/"
        }
      },
      "proposals": {
        "provider": "@putnami/github-collaboration",
        "version": 1,
        "settings": {
          "repository": "Putnami/putnami",
          "assignAuthor": true,
          "inheritLabelPrefixes": ["group/", "scope/", "area/", "severity/", "priority/"]
        }
      }
    },
    "@putnami/contributor": {
      "version": 1,
      "tasks": {
        "source": "github:putnami/putnami",
        "tiers": { "light": "tier/light", "standard": "tier/standard", "heavy": "tier/heavy" },
        "claimAssignees": ["@me"]
      },
      "proposals": { "source": "github:putnami/putnami" },
      "states": { "claimed": "in_progress", "delivered": "blocked" },
      "workers": {
        "light": "fix-light",
        "standard": "fix-standard",
        "heavy": "fix-heavy",
        "analyst": "epic-analyst"
      },
      "verification": {
        "language": "en",
        "documentation": ["sites/putnami.dev/doc/"],
        "ciGate": { "checks": ["Putnami CI"], "load": 0.7 }
      },
      "publication": { "draft": false, "taskReference": "Closes #{id}", "bodyMaxBytes": 1500 },
      "integrations": {
        "contentBump": {
          "lock": "sites/putnami.dev/content.lock.json",
          "command": ["bun", "run", "sites/putnami.dev/scripts/content-bump.ts"],
          "projects": ["putnami.dev"]
        }
      }
    }
  }
}
```

How the pieces combine for one delivery:

- `states.delivered` is `blocked`, and the GitHub provider's first `blocked`
  label is `status/needs-review`: a published fix leaves its issue awaiting
  review, as the label workflow expects. The tasks contract has no "awaiting
  review" state of its own, so `status/needs-design` reads as `blocked` too.
  An issue that is already blocked through `status/needs-design` (a reviewer
  added it after the claim, or build ran without a claim) still moves to
  `status/needs-review`: native finalization sends the transition when authorized, and the
  provider applies a state's first label whenever another label decided it.
  No mapping alone avoids that collision: the contract has three open states,
  and this repository gives four meanings to its status labels.
- `publication.taskReference` puts `Closes #<id>` in the commit and the
  proposal body; the provider reads it to copy the issue's classification
  labels (`inheritLabelPrefixes`) and GitHub closes the issue on merge when
  the mandated outcome is complete. For an intermediate unit, native finalize's
  `--partial-delivery` uses a non-closing parent reference, rejects a caller
  closing directive and skips the task's delivered transition. If a versioned
  workflow dossier reports `outcomeComplete: false`, normal parent-task
  finalization is refused. This protection also applies if the policy maps
  delivered to `done`. A partial proposal's non-closing reference may not
  trigger the provider's closing-reference label inheritance; report the
  actual labels from proposal read-back.
- `claimAssignees: ["@me"]` is the GitHub provider's notation for the
  credential's account; another provider documents its own.
- `verification.language: "en"` makes `check` and native finalization run the
  English-only detector on everything a change publishes.
- The repository squash-merges with the proposal title and body as the commit
  title and message, the merge button included. `bodyMaxBytes: 1500` keeps
  that body a commit message; native finalization posts the verification record as
  a proposal comment instead of appending it.
- Every authorized proposal opens as a draft when work starts (`putnami proposals finalize --draft`).
  Putnami CI holds runs on a draft, so checkpoint pushes cost no runner time.
- `verification.ciGate` names `Putnami CI` as the hosted check that stands for
  the gate. When the machine's load ratio exceeds `0.7`, build selects
  `proposals finalize --gate ci` instead of running a local impacted gate;
  a parent task requires `--workflow-file` and native completion eligibility before publication,
  except a partial delivery whose changed projects all declare `featureAuthority.none`;
  native finalization waits for that check on the pushed commit. It never
  runs a local gate itself.

## A minimal repository

A repository that keeps its tasks in the local provider and writes in any
language needs only the binding:

```json
{
  "extensions": ["@putnami/contributor", "@putnami/local-collaboration"],
  "agentArtifacts": ["extension:@putnami/contributor"],
  "options": {
    "collaboration": {
      "tasks": { "provider": "@putnami/local-collaboration", "version": 1 },
      "proposals": { "provider": "@putnami/local-collaboration", "version": 1 }
    }
  }
}
```

The local provider has no hosted checks and no merge: the workflows report
checks as unsupported and leave merging to the repository.

## Worker profiles and models

The shipped profiles carry a reference host model mapping in their host
metadata. To run other models, add the repository's own profiles
(`.claude/agents/<name>.md` and `.codex/agents/<name>.toml`, registered in the
host configuration where the host requires it) and name them under `workers`.
The instructions follow the policy; they never name a model.
