# ADR 0004 — a rule names the channel its publish measures against

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/ci` (`protocols/ci`), `@putnami/cli`
  (`putnami ci explain`)

## Context

A pull-request rule that publishes into `pr-{number}` republishes every member
on the first run of each pull request: the new channel has no head, so impact
is measured against nothing. `publish` lists the channels a rule advances, so
adding `canary` to it would have a pull request write the channel `main` owns.
The document needs a way to say "measure against `canary`, do not move it".

## Decision

1. **A rule names its baseline channel** in the optional member
   `rules[].baseline`. It belongs to the rule because the rule creates the
   channel that has no head.
2. **The baseline is read and never advanced.** It is not part of `publish`
   and never appears in `Explanation.publish`.
3. **The fallback order is owned by
   [`protocols/distribution` ADR 0006](../../../distribution/doc/adr/0006-a-publication-measures-against-a-channel-it-does-not-advance.md).**
   The baseline applies only when the first channel of `publish` has no head;
   the second push of a pull request measures against its own head again.
4. **One portable channel name, without `{number}`**: every pull request reads
   the same head (`validChannelName(channel, false)`).
5. **Not a channel the same rule advances.** An advanced channel is already its
   own baseline, and the single resolve would carry a duplicate. Two different
   rules naming one channel, `main` advancing `canary` and pull requests
   reading it, is the intended case.
6. **A protected channel is a valid baseline.** Protection forbids advancing a
   channel; reading its head is not moving it.
7. **A baseline with no publish is refused** with `ci.invalid_baseline` on
   `rules[i].baseline`, when the rule publishes nothing or declares
   `publish: false`.
8. **Absence** means the rule measures against its own first channel only.
9. **`putnami ci explain` reports it** with the matched rule, in the human form
   and in `Explanation.baseline`, and resolves nothing. A runner renders
   `publish` as `--channel` and `baseline` as `--baseline-channel`.

## Rejected alternatives

- **The baseline as a non-advanced entry of `publish`.** One list would mean
  two things, and every reader of `Explanation.publish` would move the channel
  the pull request must not touch.
- **A workspace-level `distribution.baseline`.** It would apply to tag rules
  and to `main`, where it means nothing, and could not differ between two
  pull-request rules.
- **The CLI flag alone.** A publication decision would live in a runner's
  configuration instead of the reviewed document that holds every other
  channel decision.
- **Infer the baseline from other rules.** Pattern-matching rules has no answer
  for a repository with two branch rules.

## Consequences

- The schema checks the alphabet; the Go validator owns the cross-field
  placement rules and decides.
- The canonical document and its digest include `baseline`.
- A workspace whose provider cannot serve a cross-channel read is not told so
  locally; `ci explain` labels every remote decision unresolved.
- `putnami.ci.json` is decoded with unknown fields refused, so a repository
  declares `baseline` only once its runner's CLI pin knows the member.
