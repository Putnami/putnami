# Navigation

Components for navigating between pages and sections.

## Breadcrumb

Path-based navigation showing the current location in a hierarchy.

```typescript
import { Breadcrumb, BreadcrumbItem, BreadcrumbLink } from '@putnami/ui';

<Breadcrumb>
  <BreadcrumbItem>
    <BreadcrumbLink href="/">Home</BreadcrumbLink>
  </BreadcrumbItem>
  <BreadcrumbItem>
    <BreadcrumbLink href="/docs">Docs</BreadcrumbLink>
  </BreadcrumbItem>
  <BreadcrumbItem>
    <BreadcrumbLink isCurrentPage>Getting Started</BreadcrumbLink>
  </BreadcrumbItem>
</Breadcrumb>
```

### Breadcrumb Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `separator` | `ReactNode` | Chevron icon | Custom separator between items |
| `spacing` | `string` | `var(--space-xs)` | Gap between items |

### BreadcrumbLink Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `href` | `string` | — | Link destination |
| `isCurrentPage` | `boolean` | — | Marks as current page (non-clickable, bold) |
| `onClick` | `() => void` | — | Click handler |

Renders a `<nav>` with `aria-label="Breadcrumb"`. The current page link uses `aria-current="page"`.

## Tabs

Tabbed interface with controlled and uncontrolled modes.

```typescript
import { Tabs, TabList, Tab, TabPanels, TabPanel } from '@putnami/ui';

<Tabs defaultIndex={0} variant="line">
  <TabList>
    <Tab>Overview</Tab>
    <Tab>Details</Tab>
    <Tab isDisabled>Coming Soon</Tab>
  </TabList>
  <TabPanels>
    <TabPanel>Overview content</TabPanel>
    <TabPanel>Details content</TabPanel>
    <TabPanel>Coming soon</TabPanel>
  </TabPanels>
</Tabs>
```

### Tabs Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `defaultIndex` | `number` | `0` | Initial active tab (uncontrolled) |
| `index` | `number` | — | Active tab (controlled) |
| `onChange` | `(index: number) => void` | — | Tab change callback |
| `variant` | `'line' \| 'enclosed' \| 'soft-rounded'` | `'line'` | Visual style |
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'` | Tab size |
| `colorScheme` | `'primary' \| 'secondary' \| 'gray'` | `'primary'` | Active tab color |

### Tab Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `isDisabled` | `boolean` | `false` | Disabled tab |

### Variant Styles

- **line**: Bottom border indicator, transparent background
- **enclosed**: Border around active tab, connected to content area
- **soft-rounded**: Pill-shaped tabs with surface background

Uses ARIA `role="tablist"`, `role="tab"`, `role="tabpanel"` with proper `aria-selected` and `tabIndex`.

## Pagination

Page navigation with ellipsis collapsing and first/last buttons.

```typescript
import { Pagination } from '@putnami/ui';

<Pagination page={3} totalPages={10} onChange={setPage} />
<Pagination page={1} totalPages={20} siblingCount={2} showFirstLast />
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `page` | `number` | — | Current page (1-indexed) |
| `totalPages` | `number` | — | Total number of pages |
| `onChange` | `(page: number) => void` | — | Page change callback |
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'` | Button size |
| `siblingCount` | `number` | `1` | Pages shown on each side of current |
| `showFirstLast` | `boolean` | `false` | Show first/last page buttons |

Returns `null` when `totalPages <= 1`. Uses `aria-label="Pagination"` and `aria-current="page"` on active button.

## Link

Styled link component built on `@putnami/web` router `Link`. Provides client-side navigation.

```tsx
import { Link } from '@putnami/ui';

<Link to="/about">About</Link>

// Prefetch the route's code + data on hover/focus
<Link to="/docs" prefetch="intent">Docs</Link>
```

Forwards `@putnami/web` `LinkProps`: set the destination with the `to` prop (not `href`), plus an optional `prefetch` behavior. Removes text decoration and uses theme text colors.
