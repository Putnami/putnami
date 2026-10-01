# Data Display

Components for presenting text, data, and visual information.

## Text

Renders styled text with responsive size, weight, color, alignment, and truncation.

```typescript
import { Text } from '@putnami/ui';

<Text size="lg" weight="bold" color="primary">Important text</Text>
<Text color="secondary" size={['sm', 'md']}>Responsive text</Text>
<Text truncate>This very long text will be truncated with an ellipsis...</Text>
<Text lineClamp={3}>Multi-line clamping after 3 lines...</Text>
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `as` | `'p' \| 'span' \| 'div' \| 'label' \| 'small' \| 'strong' \| 'em'` | `'span'` | HTML element |
| `size` | `TextSize \| TextSize[]` | `'md'` | `'xs'` through `'4xl'`, responsive |
| `weight` | `'regular' \| 'medium' \| 'bold'` | — | Font weight |
| `color` | `TextColor \| string` | `'inherit'` | `'primary'`, `'secondary'`, `'disabled'`, `'error'`, `'success'`, `'warning'`, `'info'`, or CSS color |
| `align` | `TextAlign \| TextAlign[]` | — | `'left'`, `'center'`, `'right'`, `'justify'`, responsive |
| `truncate` | `boolean` | — | Single-line ellipsis truncation |
| `lineClamp` | `number` | — | Multi-line clamping |

Also inherits all `BoxProps`.

## Heading

Semantic heading component with level-to-size mapping and responsive support.

```typescript
import { Heading } from '@putnami/ui';

<Heading level={1}>Page Title</Heading>
<Heading level={2} size="xl" color="secondary">Section</Heading>
<Heading level={3} size={['md', 'lg', 'xl']}>Responsive Heading</Heading>
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `as` | `'h1'` through `'h6'` | Derived from `level` | HTML element override |
| `level` | `1-6` | `2` | Heading level (maps to size if `size` not set) |
| `size` | `HeadingSize \| HeadingSize[]` | From level | `'xs'` through `'4xl'`, responsive |
| `weight` | `'regular' \| 'medium' \| 'bold'` | `'bold'` | Font weight |
| `color` | `'primary' \| 'secondary' \| 'disabled' \| string` | `text.primary` | Text color |
| `align` | `HeadingAlign \| HeadingAlign[]` | — | `'left'`, `'center'`, `'right'`, responsive |
| `truncate` | `boolean` | — | Ellipsis truncation |

Level-to-size mapping: 1→`3xl`, 2→`2xl`, 3→`xl`, 4→`lg`, 5→`md`, 6→`sm`.

## Badge

Inline label for status, categories, or counts.

```typescript
import { Badge } from '@putnami/ui';

<Badge colorScheme="success">Active</Badge>
<Badge variant="outline" colorScheme="error">Failed</Badge>
<Badge variant="solid" size="lg" colorScheme="primary">New</Badge>
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `variant` | `'solid' \| 'subtle' \| 'outline'` | `'subtle'` | Visual style |
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'` | Badge size |
| `colorScheme` | `'gray' \| 'primary' \| 'secondary' \| 'error' \| 'success' \| 'warning' \| 'info'` | `'gray'` | Color scheme |

## Avatar & AvatarGroup

Circular user representation with image, initials, or icon fallback.

```typescript
import { Avatar, AvatarGroup } from '@putnami/ui';

<Avatar src="/photo.jpg" name="Alice Smith" size="lg" />
<Avatar name="Bob Jones" />  {/* Shows "BJ" initials */}
<Avatar />                   {/* Shows default person icon */}

<AvatarGroup max={3} size="md">
  <Avatar name="Alice" />
  <Avatar name="Bob" />
  <Avatar name="Charlie" />
  <Avatar name="Diana" />   {/* Shows +1 overflow */}
</AvatarGroup>
```

### Avatar Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `src` | `string` | — | Image URL |
| `name` | `string` | — | Name for initials and color generation |
| `size` | `'xs' \| 'sm' \| 'md' \| 'lg' \| 'xl' \| '2xl'` | `'md'` | Avatar size (24px to 96px) |
| `icon` | `ReactNode` | Default person icon | Custom fallback icon |

Background color is deterministically generated from `name` using a hash function.

### AvatarGroup Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `max` | `number` | — | Maximum visible avatars (shows `+N` for excess) |
| `size` | `AvatarSize` | `'md'` | Size for overflow label |
| `spacing` | `string` | `'-8px'` | Overlap spacing |

## Table

Structured data display with sorting, striping, and selection.

```typescript
import { Table, Thead, Tbody, Tr, Th, Td } from '@putnami/ui';

<Table variant="striped" size="md">
  <Thead>
    <Tr>
      <Th>Name</Th>
      <Th isNumeric isSortable sortDirection="asc" onSort={handleSort}>Score</Th>
    </Tr>
  </Thead>
  <Tbody>
    <Tr>
      <Td>Alice</Td>
      <Td isNumeric>95</Td>
    </Tr>
    <Tr isSelected>
      <Td>Bob</Td>
      <Td isNumeric>87</Td>
    </Tr>
  </Tbody>
</Table>
```

### Table Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `variant` | `'simple' \| 'striped'` | `'simple'` | Table style |
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'` | Cell padding and font size |

### TableHeaderCell (Th) Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `isNumeric` | `boolean` | — | Right-align for numbers |
| `isSortable` | `boolean` | — | Show sort indicator and enable click |
| `sortDirection` | `'asc' \| 'desc' \| null` | — | Current sort direction |
| `onSort` | `() => void` | — | Sort toggle handler |

### TableCell (Td) Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `isNumeric` | `boolean` | — | Right-align with tabular numbers |

### TableRow (Tr) Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `isSelected` | `boolean` | — | Highlighted row style |

**Aliases**: `Table`, `TableHead`/`Thead`, `TableBody`/`Tbody`, `TableFoot`/`Tfoot`, `TableRow`/`Tr`, `TableCell`/`Td`, `TableHeaderCell`/`Th`.

## Card

Composable card component with header, body, and footer sections.

```typescript
import { Card, CardHeader, CardBody, CardFooter } from '@putnami/ui';

<Card variant="outline">
  <CardHeader>Settings</CardHeader>
  <CardBody>
    <Text>Configure your preferences below.</Text>
  </CardBody>
  <CardFooter>
    <Button variant="ghost">Cancel</Button>
    <Button colorScheme="primary">Save</Button>
  </CardFooter>
</Card>
```

### Card Props

Inherits all `BoxProps` plus:

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `variant` | `'elevated' \| 'outline' \| 'filled'` | `'elevated'` | Visual style |

- **elevated**: Drop shadow (`--shadow-md`)
- **outline**: 1px border
- **filled**: Surface background color

### CardHeader Props

Inherits all `BoxProps`. Renders a flex row with `space-between` alignment and a bottom border.

### CardBody Props

Inherits all `BoxProps`. Renders with `flex: 1` and padding.

### CardFooter Props

Inherits all `BoxProps`. Renders a flex row aligned to the end with a top border.

## Divider

Visual separator line, horizontal or vertical.

```typescript
import { Divider } from '@putnami/ui';

<Divider />
<Divider variant="dashed" color="var(--color-primary)" />
<Flex><div>Left</div><Divider orientation="vertical" /><div>Right</div></Flex>
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `orientation` | `'horizontal' \| 'vertical'` | `'horizontal'` | Direction |
| `variant` | `'solid' \| 'dashed' \| 'dotted'` | `'solid'` | Line style |
| `color` | `string` | `var(--color-border)` | Line color |
| `thickness` | `string` | `'1px'` | Line thickness |

## CodeBlock

Code display with syntax highlighting class and copy button.

```typescript
import { CodeBlock } from '@putnami/ui';

<CodeBlock code="const x = 42;" language="typescript" />
<CodeBlock code="echo hello" language="bash" showCopy={false} />
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `code` | `string` | — | Code content |
| `language` | `string` | `'text'` | Language label and CSS class |
| `showCopy` | `boolean` | `true` | Show copy-to-clipboard button |
