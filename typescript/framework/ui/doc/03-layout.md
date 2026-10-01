# Layout

Layout primitives provide the foundation for building page structure. All layout components support responsive props via array values and theme-based spacing/color tokens.

## Overview

- `Box` — base building block with style props (spacing, sizing, positioning, colors)
- `Flex` — flexbox container extending `Box`
- `Grid` — CSS grid container extending `Box`
- `Stack` / `HStack` / `VStack` — semantic flex layouts with gap and direction
- `Container` — centered, max-width content wrapper
- `Page` — full-height page wrapper with theme background

## Box

The foundational styled `div` that maps props directly to CSS properties. All layout components extend `Box`.

```typescript
import { Box } from '@putnami/ui';

<Box p="lg" bg="surface" borderRadius="md">
  Content with padding, background, and rounded corners
</Box>
```

### Box Props

All props accept responsive arrays: `p={['sm', 'md', 'lg']}`.

| Category | Props |
|----------|-------|
| Margin | `m`, `mt`, `mr`, `mb`, `ml`, `mx`, `my` |
| Padding | `p`, `pt`, `pr`, `pb`, `pl`, `px`, `py` |
| Sizing | `width`, `height`, `maxWidth`, `minWidth` |
| Display | `display`, `overflow`, `position`, `top`, `right`, `bottom`, `left`, `zIndex` |
| Flex | `flex`, `flexGrow`, `flexShrink`, `alignItems`, `justifyContent` |
| Color | `bg` (background), `color` (text) |
| Border | `border`, `borderRadius` |
| Text | `textAlign`, `fontSize` |

**Spacing values**: Use theme tokens (`'xs'`, `'sm'`, `'md'`, `'lg'`, `'xl'`, `'xxl'`) or numbers (multiplied by 0.25rem). Raw CSS values also work.

**Color values**: Use dot-path theme colors (`'surface'`, `'surfaceHover'`, `'text.primary'`) or CSS values.

### Polymorphism

Use the `as` prop to render any HTML element:

```typescript
<Box as="section" p="lg">Section content</Box>
<Box as="nav" bg="surface">Navigation</Box>
```

## Flex

Extends `Box` with flexbox defaults and shorthand props.

```typescript
import { Flex } from '@putnami/ui';

<Flex gap="md" align="center" justify="between" wrap="wrap">
  <div>Item 1</div>
  <div>Item 2</div>
</Flex>
```

### Flex Props

Inherits all `BoxProps` plus:

| Prop | CSS Property | Example |
|------|-------------|---------|
| `gap` | `gap` | `'md'`, `16`, `['sm', 'md']` |
| `direction` / `flexDirection` | `flex-direction` | `'row'`, `'column'` |
| `align` / `alignItems` | `align-items` | `'center'`, `'stretch'` |
| `justify` / `justifyContent` | `justify-content` | `'between'`, `'center'` |
| `wrap` / `flexWrap` | `flex-wrap` | `'wrap'`, `'nowrap'` |

## Grid

Extends `Box` with CSS grid defaults.

```typescript
import { Grid } from '@putnami/ui';

<Grid columns={3} gap="lg">
  <div>Col 1</div>
  <div>Col 2</div>
  <div>Col 3</div>
</Grid>

{/* Responsive columns */}
<Grid columns={[1, 2, 3]} gap={['sm', 'md']}>
  ...
</Grid>
```

### Grid Props

Inherits all `BoxProps` plus:

| Prop | CSS Property | Notes |
|------|-------------|-------|
| `columns` | `grid-template-columns` | Number creates `repeat(n, minmax(0, 1fr))`, string passes through |
| `rows` | `grid-template-rows` | — |
| `gap` | `gap` | Spacing tokens or CSS values |
| `areas` | `grid-template-areas` | — |
| `autoFlow` | `grid-auto-flow` | — |

## Stack / HStack / VStack

Semantic flex containers with direction, spacing, and alignment.

```typescript
import { Stack, HStack, VStack } from '@putnami/ui';

<VStack spacing="lg">
  <Heading level={2}>Title</Heading>
  <Text>Description</Text>
</VStack>

<HStack spacing="sm" align="center">
  <Avatar name="Alice" />
  <Text>Alice</Text>
</HStack>
```

### Stack Props

Inherits all `BoxProps` plus:

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `direction` | `StackDirection \| StackDirection[]` | `'column'` | `'row'`, `'column'`, `'row-reverse'`, `'column-reverse'` |
| `align` | `StackAlign` | — | `'start'`, `'center'`, `'end'`, `'stretch'`, `'baseline'` |
| `justify` | `StackJustify` | — | `'start'`, `'center'`, `'end'`, `'between'`, `'around'`, `'evenly'` |
| `spacing` | `StackSpacing \| StackSpacing[]` | `'md'` | Theme token or number |
| `wrap` | `boolean` | `false` | Enable flex wrapping |

`HStack` is `Stack` with `direction='row'` and `align='center'`.
`VStack` is `Stack` with `direction='column'`.

## Container

Centered content wrapper with responsive padding and max-width from theme breakpoints.

```typescript
import { Container } from '@putnami/ui';

<Container>
  {/* Max-width: theme.breakpoints.xl (1280px), centered */}
</Container>
```

Padding: `--space-lg` on mobile, `--space-2xl` at 768px+.

## Page

Full-height page wrapper with theme background and text color.

```typescript
import { Page } from '@putnami/ui';

<Page>
  <Container>Content</Container>
</Page>
```

## Responsive Utilities

### `responsiveCss(theme, property, value, transform?)`

Generates CSS with media queries from an array of values. Used internally by all layout components.

```typescript
responsiveCss(theme, 'padding', ['8px', '16px', '24px']);
// → padding: 8px;
//   @media (min-width: 640px) { padding: 16px; }
//   @media (min-width: 768px) { padding: 24px; }
```

### Media Query Helpers

```typescript
import { up, down, between } from '@putnami/ui';

const styles = css`
  ${up(theme, 'md')} { display: flex; }
  ${down(theme, 'sm')} { display: block; }
  ${between(theme, 'sm', 'lg')} { padding: 1rem; }
`;
```
