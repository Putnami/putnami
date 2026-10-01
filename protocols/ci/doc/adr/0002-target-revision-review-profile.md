# ADR 0002: optional review guidance in the CI document

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/ci` (`protocols/ci`), `review` member

## Context

A review plane evaluates pull requests with a model. The repository needs a
reviewed place to state what that review focuses on, without granting any
authority, and a pull request must not be able to weaken the review that
evaluates it.

## Decision

1. `putnami.ci.json` version 3 has an optional, closed `review` member:
   `enabled`, `fallbackEngine` (`codex` or `claude-code`), `focus` (one to six
   distinct areas among `correctness`, `security`, `architecture`,
   `performance`, `maintainability`, `tests`) and `instructions` (at most 32,
   each 1 to 2000 characters with no control character or surrounding
   whitespace). Every property is required inside a present profile, including
   an explicit `enabled` boolean. Unknown properties, missing fields and nulls
   are rejected with `ci.invalid_review`. A missing profile requests no review.
2. The review plane reads the profile only from the target (base) revision.
3. The framework validates and preserves the profile, order included. The
   review plane owns enrollment, policy, credentials, engine availability,
   images, tool access and publication.

## Consequences

- Review stays independent of the CI invocation and of the release-set
  provider.
- No parser, formatter or `explain` invokes a model, reads a credential, or
  grants authority.
