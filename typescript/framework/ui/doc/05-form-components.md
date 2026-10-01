# Form Components

Components for user input and interaction.

## Button

Multi-variant button with loading state, icons, and forwarded ref.

```typescript
import { Button } from '@putnami/ui';

<Button>Default</Button>
<Button colorScheme="primary" variant="solid">Primary</Button>
<Button variant="outline" colorScheme="error">Delete</Button>
<Button variant="ghost">Ghost</Button>
<Button variant="link" colorScheme="info">Learn more</Button>
<Button loading>Saving...</Button>
<Button leftIcon={<SearchIcon />} size="sm">Search</Button>
<Button fullWidth>Full Width</Button>
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `variant` | `'solid' \| 'outline' \| 'ghost' \| 'link'` | `'solid'` | Visual style |
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'` | Button size (32/40/48px height) |
| `colorScheme` | `'primary' \| 'secondary' \| 'error' \| 'success' \| 'warning' \| 'info'` | `'primary'` | Color scheme |
| `disabled` | `boolean` | `false` | Disabled state |
| `loading` | `boolean` | `false` | Loading state (shows spinner, disables interaction) |
| `fullWidth` | `boolean` | `false` | Stretch to container width |
| `leftIcon` | `ReactNode` | — | Icon before text |
| `rightIcon` | `ReactNode` | — | Icon after text |
| `type` | `'button' \| 'submit' \| 'reset'` | `'button'` | HTML button type |
| `onClick` | `MouseEventHandler` | — | Click handler |

Also inherits `BoxProps`. Supports `ref` forwarding.

### Variant Behavior

- **solid**: Filled background, contrast text, darkens on hover
- **outline**: Transparent background, colored border, light fill on hover
- **ghost**: No border, transparent background, light fill on hover
- **link**: No padding/height, underline on hover

## Input

Text input with variants, validation state, and element slots.

```typescript
import { Input } from '@putnami/ui';

<Input placeholder="Enter email" />
<Input variant="filled" size="lg" />
<Input isInvalid placeholder="Invalid field" />
<Input leftElement={<SearchIcon />} rightElement={<ClearIcon />} />
<Input variant="flushed" placeholder="Underline only" />
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `variant` | `'outline' \| 'filled' \| 'flushed'` | `'outline'` | Visual style |
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'` | Input size (32/40/48px height) |
| `isInvalid` | `boolean` | `false` | Error state (red border) |
| `isDisabled` | `boolean` | `false` | Disabled state |
| `isReadOnly` | `boolean` | `false` | Read-only state |
| `fullWidth` | `boolean` | `false` | Stretch to container width |
| `leftElement` | `ReactNode` | — | Element inside the left of the input |
| `rightElement` | `ReactNode` | — | Element inside the right of the input |

Also accepts all native `<input>` HTML attributes. Supports `ref` forwarding.

### Variant Behavior

- **outline**: Border, transparent background, focus ring
- **filled**: Filled background, transparent border, switches to outline on focus
- **flushed**: Bottom border only, no border radius

## CopyButton

Button that copies text to the clipboard with visual feedback.

```typescript
import { CopyButton } from '@putnami/ui';

<CopyButton text="Text to copy" />
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `text` | `string` | — | Text to copy to clipboard |
| `className` | `string` | — | Additional CSS class |

Shows a check icon for 2 seconds after successful copy.
