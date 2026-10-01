# @putnami/ui

React component library with Emotion, theming, dark mode, and SSR support.

## Setup

```tsx
import { ThemeProvider } from '@putnami/ui';

function App() {
  return (
    <ThemeProvider>
      {/* your app */}
    </ThemeProvider>
  );
}
```

## Components

### Layout

```tsx
import { Box, Flex, Grid, Container, VStack, HStack, Page } from '@putnami/ui';

<Container maxWidth="lg">
  <VStack gap="md">
    <HStack gap="sm">...</HStack>
  </VStack>
</Container>

<Flex direction="row" justify="space-between" align="center">...</Flex>
<Grid columns={3} gap="md">...</Grid>
```

### Typography

```tsx
import { Heading, Text } from '@putnami/ui';

<Heading level={1} size="2xl">Title</Heading>
<Text color="secondary" size="sm">Subtitle</Text>
```

### Data Display

```tsx
import { Card, CardBody, Badge, Table } from '@putnami/ui';

<Card>
  <CardBody>
    <Badge colorScheme="success">Active</Badge>
  </CardBody>
</Card>
```

### Form Controls

```tsx
import { Button, Input } from '@putnami/ui';

<Button colorScheme="primary" size="md" onClick={handleClick}>Submit</Button>
<Input placeholder="Email" type="email" />
```

### Navigation

```tsx
import { Link, Tabs, Breadcrumb } from '@putnami/ui';

// Link forwards @putnami/web LinkProps: use `to` (not `href`), optional `prefetch`
<Link to="/about">About</Link>
<Link to="/docs" prefetch="intent">Docs</Link>
```

### Feedback

```tsx
import { Alert, Spinner, Progress, useToast } from '@putnami/ui';

<Alert status="error">Something went wrong</Alert>
<Spinner size="lg" />
```

### Overlays

```tsx
import { Modal, Drawer, Tooltip, Popover } from '@putnami/ui';
```

### Icons

```tsx
import { ClockIcon, ShieldIcon, createIcon, type Icon } from '@putnami/ui';

<ClockIcon size={16} />
<ShieldIcon size={16} color="currentColor" />
```

Icons are standalone SVG components (no `lucide-react` dependency). Use the
built-in icons (`ClockIcon`, `ShieldIcon`, `WalletIcon`, …) or define your own
with `createIcon(name, nodes)`. The `Icon` type describes any icon component.

## Theming

Color palettes: `primary` (teal), `secondary` (neutral), `error`, `success`, `warning`, `info`

Spacing tokens: `xs`, `sm`, `md`, `lg`, `xl`, `xxl`

Typography sizes: `xs`, `sm`, `md`, `lg`, `xl`, `2xl`, `3xl`, `4xl`

Breakpoints: `sm` (640), `md` (768), `lg` (1024), `xl` (1280), `xxl` (1536)

### Dark Mode

Automatic via `ThemeProvider`. Persists in `localStorage`, respects `prefers-color-scheme`.

### Responsive Props

```tsx
<Box p={['sm', 'md', 'lg']}>
  {/* sm on mobile, md at 640px, lg at 1024px */}
</Box>
```

### Custom Styles (Emotion)

```tsx
import { styled, css } from '@putnami/ui';

const StyledCard = styled(Card)`
  border: 2px solid var(--color-primary);
`;
```

Colors must come from the `--color-*` custom properties, not `theme.colors.*`:
Emotion bakes the declaration into the class during SSR, where the palette is
always light, so a literal never follows a switch to dark mode. Mode-independent
tokens (spacing, radii, type ramp, breakpoints) are safe to read from the theme.

## Detailed Documentation

See `doc/` folder:
- `01-getting-started.md` — setup and overview
- `02-theming.md` — theme customization
- `03-layout.md` — layout primitives
- `04-data-display.md` — cards, tables, badges
- `05-form-components.md` — inputs, buttons, copy button
- `06-feedback.md` — alerts, toasts, spinners
- `07-overlay.md` — modals, drawers, tooltips
- `08-navigation.md` — links, tabs, breadcrumbs
- `09-markdown.md` — markdown renderer
- `10-icons.md` — icon system
- `11-emotion.md` — styled components, css prop

## SSR, interaction, and unsafe-content boundaries

- Use semantic `var(--color-*)` properties for mode-dependent colors. A literal
  read from `theme.colors.*` is baked into the SSR class and will not follow a
  later `data-color-mode` switch.
- Responsive props compile to mobile-first CSS. Do not read viewport globals in
  render to choose a layout branch.
- Overlay and tab primitives provide roles, state, keyboard behavior, focus
  bounds, and dismissal helpers. The application must still supply labels and a
  meaningful focus/content order.
- `MarkdownRenderer` sanitizes its `html` prop at the raw HTML sink.
  `MarkdownContent` does not; call `sanitizeHtml()` before using
  `dangerouslySetInnerHTML` with it.
- Client-side visibility is not authorization. Secure pages and data endpoints
  through `@putnami/web` / `@putnami/application` server middleware.

## Support and feature ownership

`@putnami/ui` is classified `stable` and owns the modeled
[`typescript/ui-system`](putnami.features.json) feature. The canonical contract
is the [UI-system specification](specs/ui-system.json), with accepted decisions
for [SSR color mode](doc/adr/0001-color-mode-is-a-css-state-not-a-render-branch.md)
and [rich-content boundaries](doc/adr/0002-rich-content-crosses-one-allowlist-boundary.md).
No default-framework or cross-language parity status is implied.
