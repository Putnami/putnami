# ADR 0001 — Treat color mode as CSS state, not a render branch

- **Status**: accepted
- **Scope**: `@putnami/ui` (`typescript/framework/ui`)

## Context

The server cannot know a first-time visitor's stored theme or system preference.
If React picks a palette during render, SSR guesses, and the browser hydrates
different markup, flashes, or keeps colors that ignore a later switch. Emotion
bakes a `theme.colors` read into the server class as a literal, which an HTML
attribute change cannot update. Responsive props have the same need: server and
browser must emit the same declarations without reading the viewport.

## Decision

- `ThemeProvider` accepts `light`, `dark`, or `system`, persists the preference,
  and emits one blocking bootstrap script that tolerates storage failures. The
  script resolves `system` and sets `data-color-mode` and `color-scheme` on
  `<html>` before application paint. Hydration reuses a matching script and
  never appends a second one.
- Mode-dependent declarations reference semantic `--color-*` custom properties.
  Light mode defines them and the dark selector replaces them, so a component
  emits the same Emotion class in both modes and recolors without remounting.
  Spacing, radii, typography, breakpoints, and raw ramps may still come from the
  typed theme object.
- Responsive array props compile to mobile-first CSS: the first element is the
  base rule, later elements map to the ordered breakpoint catalog, `null` omits a
  rule, and extra values are ignored. Rendering never reads `window.innerWidth`.

## Rejected alternatives

- **Render light on the server and correct after hydration.** A visible flash
  and possible hydration divergence.
- **Mode-dependent colors from the Emotion theme.** The literal is baked into the
  class.
- **Render both palettes and hide one.** Duplicates content, ids, focusable
  controls, and assistive output.
- **Responsive props from `window.innerWidth`.** The server has no equivalent,
  and resizing would re-render layout.

## Consequences

- New mode-dependent tokens exist as semantic custom properties, and custom
  Emotion styles use `var(--color-*)` for them.
- Component tests compare emitted classes across modes.
- The bootstrap script is inline by design; a strict content-security policy
  must allow the framework's documented bootstrap path.
