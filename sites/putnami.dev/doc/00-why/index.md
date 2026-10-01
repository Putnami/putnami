# Why Putnami

Most stacks are optimized for how fast one person can write a feature. That was
the right thing to optimize when the person who wrote a system was the person
who ran it.

Putnami makes a different bet: **the binding constraint is whether a system can
be operated by something that is not its author** — an agent, a teammate six
months later, an auditor, you at 3am. Everything else on this site follows from
that sentence.

---

## What follows from the bet

Each of these is a decision that costs something. Here is what it is paying for.

| Decision | Why it exists |
| --- | --- |
| Deterministic, machine-readable CLI output | Something has to parse it reliably, on every version, without a regex that breaks when a log line changes |
| One CLI as the front door | Humans can have many entry points. Automation needs exactly one |
| Manifest-driven declaration | A machine-readable statement of what the system *is* — not a script that rebuilds it if you already know how |
| Structured observability by construction | A human has to be able to reconstruct what happened, after the fact, without having been there |
| Tight defaults and a narrow surface | Narrow, predictable surfaces are operable. Infinite configuration space is not |
| Reversible, bounded operations | A mistake has to be cheap to undo — for the agent, and for you |
| The impact graph (`--impacted`) | Nothing should act without knowing what it affects |
| Content-addressed caching | Same input, same output, every time. Speed is the side effect |
| [Protocols](/docs/protocols) instead of shared code | A boundary that is not a contract is a boundary nobody can verify |

Read together, they are one idea. **The opinionation is not taste. It is the
shape that makes a system operable by something other than the person who built
it.**

Patterns that survive that requirement — explicit schemas, declared boundaries,
versioned intent, structured output — are the ones Putnami is built around.
Patterns that don't — ambient state, tribal knowledge, configuration that only
means something to whoever wrote it — are the ones it refuses, and that refusal
is the product.

---

## What Putnami is not

Being clear about this saves you an evaluation.

**Not a build accelerator.** Nx, Turborepo and Bazel solve the build graph well.
If a faster CI is the problem you have, use one of them. Putnami's caching
exists so that replay is deterministic; being fast is a consequence, not the
goal.

**Not an infrastructure-as-code replacement.** The `infra` contract lets a
workload declare what it needs, next to the code that needs it, and aggregates
those declarations into one manifest. It does not model your cloud account, and
it is not trying to.

**Not a PaaS with a CLI attached.** The full loop runs on your machine, with no
account. The managed platform reads the same documents you already have; it is
not the thing that makes them work.

**Not a framework you adopt for one app.** The unit Putnami operates on is the
workspace. Adopting it for a single service means paying the structure and
collecting none of the compounding.

---

## Who this is not for

If you are shipping one service and have no intention of ever delegating its
operation, the constraints below will cost you more than they return. Reach for
something lighter.

If your team's advantage is that every service gets to make its own choices,
Putnami is the opposite bet, on purpose.

And if you need a runtime escape hatch tonight, several of them are deliberately
closed. That is the point, and it will not feel like it at the time.

---

## The bill

The [principles](/docs/principles) are stated as constraints, which means each
one has a price. All of these are real:

- You cannot defer performance work to "later". Slow code is rejected early,
  even while prototyping, and convenience abstractions that hide their cost are
  not acceptable.
- You cannot change production behavior outside version control. Experiments
  require upfront modeling and explicit declaration. Quick, undocumented runtime
  tweaks are not allowed.
- You cannot bypass a security guarantee to move fast. Unsafe paths must be
  explicitly acknowledged, and some patterns stay unavailable until they can be
  secured structurally.
- You are responsible for operating — and exiting — your own primary data store.
  Some managed conveniences are intentionally unavailable.
- Machine-readable output can feel verbose to a human. Predictability is favored
  over cleverness.
- Baseline observability overhead exists, even for simple workloads. You cannot
  partially instrument the system and call it observable.
- Every dependency has to be justified.

If that reads as a cost, it is one. What it buys is a system whose behavior you
can predict, review, and hand to something that isn't you.

---

## Where this actually is today

Being honest about the gap is part of the same argument.

**The loop runs.** One CLI over a polyglot workspace: build, test, lint, serve,
package, publish, with an impact graph and content-addressed caching. Go and
TypeScript are the two full surfaces. Python is experimental: it requires an
explicit workspace opt-in, is never enabled by default, and carries no Go or
TypeScript parity promise.

**The contracts are real and tested.** The protocol layer ships schemas,
fixture corpora, and cross-language conformance — and the published
[matrix](/docs/protocols) names, per contract, what is verified by test and what
is aligned by hand.

**The managed platform is partly ahead of us.** Managed per-branch previews and
managed production deploys are product direction. What runs today runs locally,
in your CI, and against any target that can read the manifests.

**What you can depend on is published.** Not as adjectives on a marketing page —
as a reviewed catalog rendered on [Support status](/docs/support).

---

## Read next

- [Concepts](/docs/concepts) — the layers, and what each one owns.
- [Principles](/docs/principles) — the six constraints, with their tradeoffs stated in full.
- [Protocols](/docs/protocols) — how the constraints become checkable.
- [Agents](/docs/agents) — what the contracts make possible.
- [Getting started](/docs/getting-started) — the fastest way to disagree with any of this from experience.
