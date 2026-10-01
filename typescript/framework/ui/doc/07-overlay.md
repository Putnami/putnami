# Overlay Components

Components that render above the page content using portals.

## Modal

Centered dialog with overlay, keyboard handling, and scroll locking.

```typescript
import { Modal, ModalHeader, ModalBody, ModalFooter, ModalCloseButton, Button } from '@putnami/ui';

function ConfirmDialog({ isOpen, onClose }) {
  return (
    <Modal isOpen={isOpen} onClose={onClose} size="md">
      <ModalHeader>
        Confirm Action
        <ModalCloseButton onClose={onClose} />
      </ModalHeader>
      <ModalBody>Are you sure you want to proceed?</ModalBody>
      <ModalFooter>
        <Button variant="ghost" onClick={onClose}>Cancel</Button>
        <Button colorScheme="primary" onClick={handleConfirm}>Confirm</Button>
      </ModalFooter>
    </Modal>
  );
}
```

### Modal Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `isOpen` | `boolean` | — | Controls visibility |
| `onClose` | `() => void` | — | Called on close action |
| `size` | `'sm' \| 'md' \| 'lg' \| 'xl' \| 'full'` | `'md'` | Max width (400/500/700/900px/full) |
| `closeOnOverlayClick` | `boolean` | `true` | Close when clicking overlay |
| `closeOnEsc` | `boolean` | `true` | Close on Escape key |

**Sub-components**: `ModalHeader`, `ModalBody`, `ModalFooter`, `ModalCloseButton`.

**Component contract**: the sub-components forward `ref` and arbitrary DOM props (`id`, `style`, `data-*`, handlers, …) to their root element. `Modal` itself renders an overlay + dialog pair through a portal, so it has no single root: it does not accept a `ref` or spread extra props. Use `className` to style the dialog panel and the props above to control behavior.

## Drawer

Sliding panel from any edge of the screen.

```typescript
import { Drawer, DrawerHeader, DrawerBody, DrawerFooter, DrawerCloseButton } from '@putnami/ui';

<Drawer isOpen={isOpen} onClose={onClose} placement="right" size="md">
  <DrawerHeader>
    Settings
    <DrawerCloseButton onClose={onClose} />
  </DrawerHeader>
  <DrawerBody>Drawer content</DrawerBody>
  <DrawerFooter>
    <Button onClick={onClose}>Close</Button>
  </DrawerFooter>
</Drawer>
```

### Drawer Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `isOpen` | `boolean` | — | Controls visibility |
| `onClose` | `() => void` | — | Called on close action |
| `placement` | `'left' \| 'right' \| 'top' \| 'bottom'` | `'right'` | Slide direction |
| `size` | `'xs' \| 'sm' \| 'md' \| 'lg' \| 'xl' \| 'full'` | `'md'` | Panel size |
| `closeOnOverlayClick` | `boolean` | `true` | Close when clicking overlay |
| `closeOnEsc` | `boolean` | `true` | Close on Escape key |

**Horizontal sizes**: xs=256px, sm=320px, md=400px, lg=512px, xl=640px.
**Vertical sizes**: xs=200px, sm=300px, md=400px, lg=500px, xl=600px.

**Sub-components**: `DrawerHeader`, `DrawerBody`, `DrawerFooter`, `DrawerCloseButton`.

**Component contract**: the sub-components forward `ref` and arbitrary DOM props (`id`, `style`, `data-*`, handlers, …) to their root element. `Drawer` itself renders an overlay + panel pair through a portal, so it has no single root: it does not accept a `ref` or spread extra props. Use `className` to style the panel and the props above to control behavior.

## Dropdown

Contextual menu with trigger, items, and keyboard support.

```typescript
import { Dropdown, DropdownTrigger, DropdownMenu, DropdownItem, DropdownDivider, Button } from '@putnami/ui';

<Dropdown placement="bottom-start">
  <DropdownTrigger>
    <Button>Options</Button>
  </DropdownTrigger>
  <DropdownMenu>
    <DropdownItem icon={<EditIcon />} onClick={handleEdit}>Edit</DropdownItem>
    <DropdownItem onClick={handleDuplicate}>Duplicate</DropdownItem>
    <DropdownDivider />
    <DropdownItem isDanger onClick={handleDelete}>Delete</DropdownItem>
  </DropdownMenu>
</Dropdown>
```

### Dropdown Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `placement` | `'bottom-start' \| 'bottom-end' \| 'top-start' \| 'top-end'` | `'bottom-start'` | Menu position |
| `offset` | `number` | `4` | Pixel offset from trigger |
| `closeOnSelect` | `boolean` | `true` | Close menu when an item is clicked |

### DropdownItem Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `icon` | `ReactNode` | — | Leading icon |
| `isDisabled` | `boolean` | `false` | Disabled state |
| `isDanger` | `boolean` | `false` | Red destructive styling |
| `onClick` | `() => void` | — | Click handler |

The menu auto-positions to stay within viewport bounds. Closes on outside click and Escape.

## Popover

Rich content popup attached to a trigger element.

```typescript
import { Popover, PopoverHeader, PopoverBody, PopoverCloseButton, Button } from '@putnami/ui';

<Popover
  placement="bottom"
  trigger="click"
  content={
    <>
      <PopoverHeader>Details</PopoverHeader>
      <PopoverBody>More information here.</PopoverBody>
    </>
  }
>
  <Button variant="outline">Show Info</Button>
</Popover>

{/* Hover trigger */}
<Popover trigger="hover" placement="top" content={<PopoverBody>Quick info</PopoverBody>}>
  <span>Hover me</span>
</Popover>
```

### Popover Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `placement` | `'top' \| 'bottom' \| 'left' \| 'right'` | `'bottom'` | Position relative to trigger |
| `trigger` | `'click' \| 'hover'` | `'click'` | How to open |
| `offset` | `number` | `12` | Pixel offset |
| `isOpen` | `boolean` | — | Controlled open state |
| `onOpen` | `() => void` | — | Open callback |
| `onClose` | `() => void` | — | Close callback |
| `closeOnBlur` | `boolean` | `true` | Close on outside click |
| `content` | `ReactNode` | — | Popover content |

Renders with an arrow pointing to the trigger. Supports controlled and uncontrolled modes.

**Sub-components**: `PopoverHeader`, `PopoverBody`, `PopoverCloseButton`.

**Component contract**: the sub-components forward `ref` and arbitrary DOM props (`id`, `style`, `data-*`, handlers, …) to their root element. `Popover` itself wraps a trigger and renders its content through a portal, so it has no single root: it does not accept a `ref` or spread extra props. Use `className` to style the floating panel and the props above to control behavior.

## Tooltip

Lightweight text popup on hover/focus.

```typescript
import { Tooltip, Button } from '@putnami/ui';

<Tooltip label="Save your changes" placement="top">
  <Button>Save</Button>
</Tooltip>

<Tooltip label="Coming soon" isDisabled={false}>
  <span>Feature</span>
</Tooltip>
```

### Props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `label` | `ReactNode` | — | Tooltip content |
| `placement` | `'top' \| 'bottom' \| 'left' \| 'right'` | `'top'` | Position |
| `delay` | `number` | `200` | Show delay in ms |
| `offset` | `number` | `8` | Pixel offset |
| `isDisabled` | `boolean` | `false` | Disable tooltip |

Renders via portal with arrow, viewport boundary detection, and fade-in animation. Supports both hover and focus triggers.
