# ADR 0002 — Make rich content cross one allowlist boundary

- **Status**: accepted
- **Scope**: `@putnami/ui` (`typescript/framework/ui`)

## Context

Content often reaches React as rendered HTML, which needs a raw HTML sink. That
HTML can carry scripts, event attributes, CSS injection, embedded browsing
contexts, or URL schemes hidden behind entities and control characters; a
blacklist grows one bypass at a time. Documentation still needs headings, lists,
tables, code-highlighting attributes, Mermaid and code-group containers, anchors,
and accessibility attributes.

## Decision

- `MarkdownRenderer` passes every `html` prop through the exported
  `sanitizeHtml()` immediately before the raw HTML sink. The sanitizer is a
  dependency-free allowlist of the elements and attributes the document
  components need.
- Executable and embedding elements, `<style>`, inline handlers, comments, and
  inline `style` are removed. Script and style contents go with their container;
  other disallowed elements are unwrapped so their text survives.
- URLs are entity-decoded and stripped of whitespace and control characters
  before the scheme check. Relative URLs and the documented safe schemes pass;
  non-image `data:`, `javascript:`, and `vbscript:` are rejected. A safe subset
  of Shiki custom properties survives on code markup only.
- `target="_blank"` links get `rel="noopener noreferrer"`. `data-*` and `aria-*`
  attributes survive.
- Mermaid SVG is the one sink that bypasses `sanitizeHtml()`, whose allowlist is
  HTML-only. It is accepted only because Mermaid is loaded with an integrity
  check and pinned to `securityLevel: 'strict'`.
- `MarkdownContent` is a styled container with no hidden sanitization; callers
  that use its raw HTML sink call `sanitizeHtml()` themselves.
- Interactive overlays expose dialog semantics while open, keep Tab inside
  eligible nodes, and dismiss on Escape or outside click. Callers provide labels
  and a sensible initial trigger.

## Rejected alternatives

- **Trust the markdown parser.** Its configuration changes and raw HTML passes
  through; the component that owns the sink owns its safety.
- **Blacklist dangerous strings.** Encoding, case, whitespace, controls, and
  malformed tags make the bypass list unbounded.
- **Strip every attribute.** Loses ids, accessibility, highlighting, and code
  groups.
- **Sanitize only in the site loader.** Other `@putnami/ui` consumers would get a
  raw sink.
- **Sanitize `MarkdownContent` silently.** It is not an HTML parser, and a hidden
  transform would not protect direct `dangerouslySetInnerHTML` use.
- **Extend the allowlist to Mermaid's SVG.** Admitting the full SVG surface would
  break diagrams or reopen the XSS hole.

## Consequences

- New markup needs an allowlist change and hostile-input regression tests.
- This is sanitization, not isolation; arbitrary active content needs a
  sandboxed origin or iframe.
- Primitives provide roles, state, focus bounds, and keyboard behavior; callers
  provide labels and content order.
