# Getting Started

`@putnami/ui` is a React component library built on [Emotion](https://emotion.sh) with built-in theming, dark mode, responsive utilities, and SSR support. It provides layout primitives, data display components, form controls, overlays, and feedback elements.

## Installation

```bash
putnami deps add @putnami/ui
```

`@putnami/ui` depends on `@putnami/web` (for routing and SSR) and `@putnami/runtime`.

## Quick Start

Wrap your application with `ThemeProvider` to enable theming and global styles:

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

## Package Structure

The library exports five modules:

| Module | Purpose |
|--------|---------|
| `theme` | Theme definition, provider, color mode, global styles |
| `layout` | `Box`, `Flex`, `Grid`, `Container`, `Page` primitives |
| `components` | 28 UI components (buttons, cards, tables, overlays, etc.) |
| `icons` | `Icon` type and pre-exported Lucide icons |
| `emotion` | Re-exported Emotion primitives (`styled`, `css`, `Global`, `keyframes`) |

## Key Concepts

### Theming

All components consume the theme via Emotion's `ThemeProvider`. The default theme includes:
- **Color palettes**: primary (teal), secondary (neutral), error, success, warning, info — each with a full 50-900 scale plus semantic `main`/`light`/`dark`/`contrastText`
- **Spacing**: token-based (`xs` through `xxl`) or numeric factor (e.g., `spacing(4)` = `1rem`)
- **Typography**: font families (sans + mono), sizes (`xs` through `4xl`), weights, heading styles
- **Breakpoints**: `sm` (640), `md` (768), `lg` (1024), `xl` (1280), `xxl` (1536)

See [Theming](./02-theming.md) for details.

### Dark Mode

`ThemeProvider` handles dark mode automatically:
- Persists user preference in `localStorage`
- Respects `prefers-color-scheme` when set to `system`
- Injects a blocking script during SSR to prevent flash of wrong theme
- Uses CSS custom properties so all colors update without re-rendering components

### Responsive Design

Layout props accept arrays for responsive values. The first value is the base (mobile), subsequent values apply at breakpoints `sm`, `md`, `lg`, `xl`:

```typescript
<Box p={['sm', 'md', 'lg']}>
  {/* padding: sm on mobile, md at 640px, lg at 1024px */}
</Box>

<Heading size={['lg', 'xl', '2xl']}>Responsive Heading</Heading>
```

### SSR Support

The library works with `@putnami/web` SSR out of the box:
- `ThemeProvider` creates a per-request Emotion cache with `compat: true`
- Styles are extracted after rendering and injected into `<head>`
- A blocking color-mode script prevents theme flash on first load
