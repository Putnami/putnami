# Markdown

Components for rendering and navigating markdown content.

## MarkdownContent

Styled container for rendered markdown HTML. Provides consistent typography, code blocks, tables, blockquotes, images, and support for Shiki syntax highlighting and Mermaid diagrams.

```typescript
import { MarkdownContent, sanitizeHtml } from '@putnami/ui';

<MarkdownContent dangerouslySetInnerHTML={{ __html: sanitizeHtml(htmlContent) }} />
```

> **Security**: `MarkdownContent` is a styled `div` and does **not** sanitize its content.
> When you inject an HTML string with `dangerouslySetInnerHTML`, run it through
> `sanitizeHtml` first (shown above) or use [`MarkdownRenderer`](#markdownrenderer), which
> sanitizes automatically. Never inject unsanitized, user-controlled HTML.

This is a styled `div` (not a component with props). Apply it by wrapping rendered HTML. It styles:

- **Headings**: h1-h6 with appropriate sizes and margins
- **Code**: Inline `code` with background and border; `pre` blocks with overflow
- **Tables**: Full-width, collapsed borders, header backgrounds
- **Links**: Info-colored with hover underline
- **Blockquotes**: Left border, muted italic text
- **Images**: Max-width, rounded corners
- **Code groups**: Tabbed code blocks for polyglot content (`.code-group` class)
- **Mermaid diagrams**: Loading state, error display (`.mermaid-diagram` class)
- **Copy buttons**: Hover-reveal copy buttons on code blocks (`.code-wrapper` class)

## MarkdownRenderer

Full-featured markdown rendering component with interactive features:
- Client-side navigation for internal links (via `@putnami/web` router)
- Auto-injected copy buttons on all code blocks
- Code group tab switching with persistent language preference
- Mermaid diagram rendering (lazy-loaded from CDN)

```typescript
import { MarkdownRenderer } from '@putnami/ui';

<MarkdownRenderer html={renderedMarkdownHtml} />
```

### Props

| Prop | Type | Description |
|------|------|-------------|
| `html` | `string` | Pre-rendered markdown HTML |

> **Security**: `MarkdownRenderer` runs the `html` prop through a strict, dependency-free
> allowlist sanitizer (`sanitizeHtml`) before injecting it. Script tags, inline event
> handlers (`on*`), and dangerous URL schemes (`javascript:`, non-image `data:`) are removed,
> while the structural markup a markdown processor emits — headings, lists, tables, code
> blocks, links, images, and mermaid/code-group containers — is preserved. You can apply the
> same sanitizer yourself via the exported `sanitizeHtml(html)` helper.

**Internal links** (starting with `/`) are intercepted and use client-side navigation instead of full page reloads.

**Code groups** sync across the page: clicking "TypeScript" in one code group switches all groups that have a TypeScript tab. The preference is saved in `localStorage` under `putnami-code-lang`.

**Mermaid diagrams** are rendered client-side by loading Mermaid from a CDN. Diagrams show "Loading diagram..." until rendered, and display error messages if rendering fails.

## MarkdownToc

Sticky table of contents sidebar with scroll-spy highlighting.

```typescript
import { MarkdownToc, type TocItem } from '@putnami/ui';

const toc: TocItem[] = [
  { level: 1, id: 'introduction', text: 'Introduction' },
  { level: 2, id: 'installation', text: 'Installation' },
  { level: 2, id: 'usage', text: 'Usage' },
];

<MarkdownToc items={toc} />
```

### Props

| Prop | Type | Description |
|------|------|-------------|
| `items` | `TocItem[]` | List of headings with level, id, and text |

### TocItem

| Field | Type | Description |
|-------|------|-------------|
| `level` | `number` | Heading level (1-6), controls indentation |
| `id` | `string` | HTML id of the heading element |
| `text` | `string` | Display text |

### TopicDoc

Convenience type for markdown content with its table of contents:

```typescript
interface TopicDoc {
  content: string;  // Rendered HTML
  toc: TocItem[];   // Table of contents
}
```

### Behavior

- **Sticky positioning**: Sticks below the navbar (top: 88px)
- **Scroll spy**: Uses `IntersectionObserver` to highlight the current section
- **Active indicator**: Blue bar and slight indentation on the active item
- **Responsive**: Hidden on mobile/tablet, visible from `lg` breakpoint
- **Indentation**: Items are indented based on heading level
