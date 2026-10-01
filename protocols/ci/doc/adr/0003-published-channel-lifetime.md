# ADR 0003 — a published channel declares its lifetime

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/ci` (`protocols/ci`), `@putnami/cli`
  (`putnami ci explain`)

## Context

A rule such as `{ "pullRequests": true, "publish": ["pr-{number}"] }` creates a
channel per pull request, and a branch rule can create short-lived channels.
Without a declared lifetime, each leaves a channel head behind for good. A
lifetime is repository intent, reviewed beside the rule that creates the
channel. A provider should not guess it from a channel name, and the framework
cannot execute it: it owns no channel and retracts nothing.

## Decision

1. **A rule declares the lifetime of the channels it publishes** in the
   optional member `rules[].retain`. It applies to every channel in the rule's
   `publish` list.
2. **One keyword, one duration spelling.**
   - `"while-open"`: the provider is expected to retract the channel head when
     the pull request closes or merges. It is refused on a `branches` or `tags`
     rule, and refused unless every channel the rule publishes carries
     `{number}`: one channel shared by several pull requests would be
     retracted when the first one closes. A shared channel takes a day count.
   - `"<n>d"`: a whole number of days, `1d` to `365d`, counted from the
     channel's last accepted move; then the provider is expected to expire the
     head.
3. **Days are the only unit.** One intent has one spelling and therefore one
   canonical digest, with no normalization of the author's file. A channel
   meant to outlive a year is not ephemeral; declare it in
   `distribution.channels`.
4. **A lifetime with no channel is refused** with `ci.invalid_retain` on
   `rules[i].retain`: a rule that publishes nothing or declares
   `publish: false` cannot apply it.
5. **Absence** declares no lifetime: the channel is kept until someone removes
   it.
6. **The contract says what is expected, never how.** Retraction and expiry
   name a channel head; the immutable sets it pointed at may still be
   referenced elsewhere. When and how a provider reaps is the provider's part.
7. **`putnami ci explain` reports the lifetime** with the matched rule, in the
   human form and in `Explanation.retain`, and says so when none is declared.

## Rejected alternatives

- **`expireAfter` beside a `retract` keyword.** Two members for one quantity
  need a "not both" rule. One member carrying a keyword or a duration follows
  `rollout.advance`.
- **A Go duration.** `time.ParseDuration` has no day unit and accepts `1ns`
  and `0s`; authors would convert days to hours by hand.
- **An ISO 8601 duration (`P30D`).** A spelling no other member of this
  document uses, for a range nobody needs.
- **A provider-defined free-form string.** It cannot be validated and defers
  the decision to every implementation.
- **Pull-request channels ephemeral by construction.** It hard-codes one
  policy: a channel could not survive a merge for a day of testing, and branch
  channels would still have no answer.
- **The lifetime on the distribution wire (`ChannelRequest`).** No producer
  reads `retain` per release; the declaration already lives in the document
  the execution plane reads.

## Consequences

- The schema checks the alphabet; the Go validator owns the numeric bound and
  the cross-field placement rules, which a JSON Schema regex cannot express.
  The validator decides.
- The canonical document and its digest include `retain`.
- A workspace that declares a lifetime its provider does not implement is not
  told so locally; `ci explain` labels every remote decision unresolved.
