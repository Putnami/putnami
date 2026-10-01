# @putnami/ui

React component library with Emotion styling, built-in theming, dark mode, and SSR support.

## Features

- Theme system with light/dark mode, CSS custom properties, and SSR-safe rendering
- Layout primitives: `Box`, `Flex`, `Grid`, `Stack`, `Container`
- 28 components: buttons, cards, tables, modals, drawers, dropdowns, tabs, toasts, and more
- Responsive props via array values (mobile-first breakpoints)
- Self-contained SVG icons and a `createIcon` helper
- Emotion re-exports (`styled`, `css`, `keyframes`) with typed theme
- Dialog, focus-trap, keyboard-navigation, and busy-state semantics for interactive primitives
- Allowlist-sanitized rich HTML through `MarkdownRenderer` and the exported `sanitizeHtml()` helper

## Installation

```bash
putnami deps add @putnami/ui
```

## Quick Start

```typescript
import { ThemeProvider, Container, Heading, Text, Button } from '@putnami/ui';

function App() {
  return (
    <ThemeProvider>
      <Container>
        <Heading level={1}>Hello, Putnami</Heading>
        <Text color="secondary">A full-featured UI component library.</Text>
        <Button colorScheme="primary" onClick={() => alert('Clicked!')}>
          Get Started
        </Button>
      </Container>
    </ThemeProvider>
  );
}
```

## Documentation

- **[Getting Started](doc/01-getting-started.md)** — Installation, setup, key concepts
- **[Theming](doc/02-theming.md)** — Theme customization, color mode, CSS variables
- **[Layout](doc/03-layout.md)** — Box, Flex, Grid, Stack, Container, responsive utilities
- **[Data Display](doc/04-data-display.md)** — Text, Heading, Badge, Avatar, Card, Table, Divider, CodeBlock
- **[Form Components](doc/05-form-components.md)** — Button, Input, CopyButton
- **[Feedback](doc/06-feedback.md)** — Alert, ToastProvider, useToast, Progress, Spinner
- **[Overlay](doc/07-overlay.md)** — Modal, Drawer, Dropdown, Popover, Tooltip
- **[Navigation](doc/08-navigation.md)** — Breadcrumb, Tabs, Pagination, Link
- **[Markdown](doc/09-markdown.md)** — MarkdownContent, MarkdownRenderer, MarkdownToc
- **[Icons](doc/10-icons.md)** — Built-in SVG icons, Icon type, and createIcon
- **[Emotion](doc/11-emotion.md)** — styled, css, keyframes with theme integration

## API Overview

| Export | Description |
|--------|-------------|
| `ThemeProvider` | Theme and color mode provider |
| `useTheme` | Access the current theme |
| `useColorMode` | Get/set color mode (light/dark/system) |
| `Box` / `Flex` / `Grid` | Layout primitives with style props |
| `Stack` / `HStack` / `VStack` | Directional flex containers |
| `Button` / `Input` | Form controls |
| `Alert` / `ToastProvider` / `useToast` / `Progress` | Feedback utilities |
| `Modal` / `Drawer` / `Dropdown` | Overlay components |
| `Tabs` / `Breadcrumb` / `Pagination` | Navigation components |
| `Card` / `Table` / `Badge` / `Avatar` | Data display components |
| `styled` / `css` / `keyframes` | Emotion utilities with typed theme |

## Browser and server behavior

Components render on the server without reading browser globals during render.
`ThemeProvider` emits the color-mode bootstrap script needed to resolve a
persisted or system preference before paint; components use semantic CSS custom
properties so hydration and later mode switches keep the same Emotion classes.

Interactive semantics are a component contract, not a complete application
accessibility audit. Callers still provide labels, meaningful content order,
and an appropriate initial focus target. `MarkdownRenderer` sanitizes its HTML
prop; `MarkdownContent` is only a styled container, so raw HTML passed to it must
first go through `sanitizeHtml()`.

## Support and contract

`@putnami/ui` is a public, documented, maintained package classified `stable`.
The [UI-system specification](specs/ui-system.json),
[color-mode ADR](doc/adr/0001-color-mode-is-a-css-state-not-a-render-branch.md),
and [rich-content ADR](doc/adr/0002-rich-content-crosses-one-allowlist-boundary.md)
define its SSR, responsive, interaction, and sanitization boundaries.

Routing and server authorization remain owned by `@putnami/web` and
`@putnami/application`; UI visibility is never an access-control decision. The
package makes no default-framework or cross-language parity claim. Before
v1.0.0, minor `0.x` releases may contain documented breaking changes.
