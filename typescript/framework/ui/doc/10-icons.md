# Icons

Built-in SVG icon components and a helper for defining your own.

## Overview

`@putnami/ui` ships a small set of self-contained SVG icon components plus a
`createIcon` helper. There is **no `lucide-react` dependency** — each icon is a
standalone React component built from SVG path data. The `Icon` type describes
any icon component, and `LucideIcon` is kept as a compatibility alias for code
that expects Lucide's type.

## Icon Type

```typescript
import type { Icon } from '@putnami/ui';

// Use to type icon props in your components
interface MyComponentProps {
  icon: Icon;
}
```

`Icon` is a `ForwardRefExoticComponent` of `IconProps` (`size`, `color`,
`strokeWidth`, `absoluteStrokeWidth`, plus standard SVG props). `LucideIcon` is
an alias of `Icon`.

## Built-in Icons

These icons are exported directly from `@putnami/ui`:

| Export | Common Use |
|--------|------------|
| `TrendingUpIcon` | Growth, analytics |
| `WalletIcon` | Finance, payments |
| `ClockIcon` | Time, scheduling |
| `TargetIcon` | Goals, objectives |
| `ShieldIcon` | Security, protection |
| `ZapIcon` | Performance, speed |
| `DropletsIcon` | Resources, water |
| `LogOutIcon` | Sign out, exit |

```tsx
import { ClockIcon } from '@putnami/ui';

<ClockIcon size={16} />
```

## Creating Custom Icons

For icons that aren't built in, define your own with `createIcon(name, nodes)`,
where `nodes` is an array of `[tag, attributes]` SVG primitives:

```tsx
import { createIcon } from '@putnami/ui';

const SearchIcon = createIcon('search', [
  ['circle', { cx: 11, cy: 11, r: 8 }],
  ['path', { d: 'm21 21-4.3-4.3' }],
]);

<Button leftIcon={<SearchIcon size={16} />}>Search</Button>
```

All components that accept icon props (`Button.leftIcon`, `Alert.icon`,
`DropdownItem.icon`, etc.) accept any `ReactNode`, so icon components work
directly.

## Styling Icons

Icons inherit `currentColor` and can be sized via the `size` prop (or CSS):

```tsx
<ClockIcon size={20} strokeWidth={2} />
```

Components that render icons typically set icon dimensions via CSS
(`width: 1em; height: 1em`), so icons scale with the component's font size.
