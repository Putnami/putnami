# ADR 0041 — Repository scanners declare Git candidate inputs

- **Status**: accepted
- **Scope**: CLI cache inputs (`internal/store`, `internal/git`), workspace
  impact, repository document gates

## Context

Repository scanners, such as the public-cut gate and the release rehearsal,
enumerate the whole repository with
`git ls-files --cached --others --exclude-standard`. A fixed test-input list
misses documents in unrelated projects, so a new finding can leave both
impacted selection and the cached verdict unchanged. A filesystem-wide glob
reads ignored local worktrees and private state the scanner excludes.

## Decision

- The file-pattern grammar has an explicit `git:` collection source. It
  enumerates the containing repository's candidate paths through one shared
  NUL-delimited, sorted, deduplicated Git helper (`git.CandidatePaths`).
  Patterns match normalized project-relative paths; `git:**` covers the
  repository.
- Impact uses the same grammar without checking current membership, so deleted
  and renamed paths select their readers too.
- Candidate bytes are hashed raw, including config fields ordinary inputs may
  normalize. A symlink's target text is hashed without following it, with a
  type marker that separates it from a regular file with identical bytes. Raw
  candidate reads take precedence when ordinary patterns overlap.
- An enumeration failure or an unsupported non-regular candidate produces no
  key.
- A repository scanner declares `git:**` (the documents project does) and
  shares candidate enumeration with its scan, so its key and its scan read one
  set.

## Consequences

- Every candidate edit can re-run the document suite.
- Ignored untracked noise cannot change the digest; tracked ignored files stay
  covered. Staging a candidate changes no bytes and no identity.
- A workspace using `git:` inputs requires Git and a repository. Ordinary
  filesystem patterns keep their behavior.
