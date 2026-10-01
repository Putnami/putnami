# Feedback

Components for communicating status, progress, and notifications to users.

## Alert

Status messages with icon, title, description, and optional close button.

```typescript
import { Alert } from '@putnami/ui';

<Alert status="success" title="Saved">Your changes have been saved.</Alert>
<Alert status="error" variant="solid">Something went wrong.</Alert>
<Alert status="warning" variant="left-accent" onClose={() => dismiss()}>
  Your session expires in 5 minutes.
</Alert>
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `status` | `'info' \| 'success' \| 'warning' \| 'error'` | `'info'` | Alert type (determines color and default icon) |
| `variant` | `'subtle' \| 'solid' \| 'left-accent' \| 'top-accent'` | `'subtle'` | Visual style |
| `title` | `ReactNode` | — | Bold title text |
| `icon` | `ReactNode` | Auto from status | Custom icon (set to `null` to hide) |
| `onClose` | `() => void` | — | Show close button and call on click |

**Component contract**: `Alert` forwards `ref` and any extra DOM props (`id`, `style`, `data-*`, event handlers, …) to its root `<div>`. The `title` prop is widened to `ReactNode`, overriding the native `title` attribute.

## Toast

Temporary notification system with positioning and auto-dismiss.

### Setup

Wrap your app with `ToastProvider`:

```typescript
import { ToastProvider } from '@putnami/ui';

<ThemeProvider>
  <ToastProvider defaultPosition="top-right">
    <App />
  </ToastProvider>
</ThemeProvider>
```

### Usage

```typescript
import { useToast } from '@putnami/ui';

function SaveButton() {
  const { toast, closeAll } = useToast();

  const handleSave = () => {
    toast({
      title: 'Saved',
      description: 'Your changes have been saved.',
      status: 'success',
      duration: 5000,
    });
  };
}
```

### ToastProvider Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `defaultPosition` | `ToastPosition` | `'top-right'` | Default position for toasts |

### `useToast()` Return Value

| Method | Type | Description |
|--------|------|-------------|
| `toast` | `(options: ToastOptions) => string` | Show a toast, returns its ID |
| `closeToast` | `(id: string) => void` | Dismiss a specific toast |
| `closeAll` | `() => void` | Dismiss all toasts |

### ToastOptions

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `title` | `string` | — | Toast title |
| `description` | `string` | — | Toast description |
| `status` | `'info' \| 'success' \| 'warning' \| 'error'` | `'info'` | Toast type |
| `duration` | `number` | `5000` | Auto-dismiss in ms (0 = no auto-dismiss) |
| `isClosable` | `boolean` | `true` | Show close button |
| `position` | `ToastPosition` | Provider default | `'top'`, `'top-right'`, `'top-left'`, `'bottom'`, `'bottom-right'`, `'bottom-left'` |

## Progress

Horizontal progress bar with determinate and indeterminate modes.

```typescript
import { Progress } from '@putnami/ui';

<Progress value={65} />
<Progress value={80} colorScheme="success" size="lg" hasStripe isAnimated />
<Progress isIndeterminate colorScheme="info" />
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `value` | `number` | `0` | Current value |
| `max` | `number` | `100` | Maximum value |
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'` | Bar height (4/8/12px) |
| `colorScheme` | `'primary' \| 'secondary' \| 'success' \| 'warning' \| 'error' \| 'info'` | `'primary'` | Bar color |
| `isIndeterminate` | `boolean` | `false` | Animated indeterminate state |
| `hasStripe` | `boolean` | `false` | Striped pattern |
| `isAnimated` | `boolean` | `false` | Animate stripes (only with `hasStripe`) |
| `aria-label` | `string` | — | Accessible label |

## Spinner

Animated loading indicator.

```typescript
import { Spinner } from '@putnami/ui';

<Spinner />
<Spinner size="lg" color="var(--color-primary)" />
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'` | Spinner size (14/18/24px) |
| `color` | `string` | `currentColor` | Spinner color |
