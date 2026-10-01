# ADR 0002 — The system model is a navigation axis, not a surface

- **Status**: accepted
- **Scope**: `putnami.dev` (`sites/putnami.dev`)

## Context

Product surfaces (Tooling, TypeScript, Go, Python, Platform) answer "where do I
work". They cannot answer "what is this system, and why should I trust it": the
protocol layer, the agent-operability surface and the reasoning behind the
constraints belong to no surface. Rendering those pages as cards inside the
surface grid teaches a reader that "Concepts" is a sixth surface beside Go.

## Decision

The site has two navigation axes.

**The system model** (Why Putnami, Concepts & principles, Protocols,
Spec-driven development, Agents) is read once and applies everywhere. It is its
own row on the docs hub, above the surface grid, and its pages are first-level
nav entries, in reading order: the bet, the layers, the contracts that make the
layers checkable, and what those contracts enable.

**Product surfaces** are chosen per task, and the sidebar narrows after that
choice.

Three constraints follow.

**A model page is never a `TOOLS` entry.** It has no glyph, no page count and
no support subject. `protocols/support` classifies things a reader depends on;
a badge on a page describing the system would invent a promise no catalog
holds ([ADR 0001](0001-the-support-catalog-is-rendered-never-restated.md)).

**Python is browseable, never a first-level anchor.** `TOOL_ORDER` drives every
browseable listing (docs hub, home strip, surface menu, command palette) and
keeps Python between Go and the Platform. In the navbar, the languages share
one Frameworks menu, and Python never gets an anchor of its own: that would
imply a parity the workspace does not offer. Every surface presenting Python
states that it is experimental, opt-in, not default, and without Go or
TypeScript parity.

**Model pages are documentation, not marketing routes.** They live under
`/docs/`, so search indexes them, `llms.txt` lists them, and *Copy as Markdown*
and the raw-markdown endpoint serve them: an agent can read them.

## Consequences

- Published URLs ignore the numeric section prefixes (`toUrlPath` strips them),
  but asset entries in `putnami.json` follow the folders, so renumbering a
  section means clearing stale numbered directories under `.gen/public/docs`
  before a rebuild, or the section renders twice.
- A new model page is two hand edits: `MODEL_CARDS` in `src/app/docs/page.tsx`,
  and `SYSTEM_LINKS` plus the nav links in `src/components/navbar.tsx`. Nothing
  auto-registers.

## Alternatives considered

- **Cross-cutting pages as cards in the surface grid.** The grid mixes "a
  language you write in" with "the reasoning behind the system" and ranks the
  model pages as leftovers.
- **Model pages as top-level routes outside `/docs/`.** Excludes them from
  search, `llms.txt` and *Copy as Markdown*, the machine-readable surfaces the
  pages describe.
- **Fold Protocols and Agents into Concepts.** Concepts maps the layers; the
  protocol layer is the evidence the map is enforced, and the agent surface is
  what the evidence buys. One merged page is one page nobody finishes.
