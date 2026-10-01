# Emotion Integration

`@putnami/ui` re-exports Emotion's core utilities augmented with the Putnami theme type, so you can build custom styled components that integrate seamlessly with the theme system.

## Exports

```typescript
import { styled, css, Global, keyframes } from '@putnami/ui';
```

| Export | From | Description |
|--------|------|-------------|
| `styled` | `@emotion/styled` | Create styled components with theme access |
| `css` | `@emotion/react` | Create CSS blocks with theme interpolation |
| `Global` | `@emotion/react` | Inject global CSS |
| `keyframes` | `@emotion/react` | Define CSS animations |

## Theme Augmentation

The Emotion `Theme` interface is augmented to include the full Putnami theme shape:

```typescript
declare module '@emotion/react' {
  export interface Theme {
    colorMode: ResolvedColorMode;
    colors: ThemeColors;
    spacing: (factor: number) => string;
    space: { xs, sm, md, lg, xl, xxl };
    breakpoints: { sm, md, lg, xl, xxl };
    typography: ThemeTypography;
    shadows: { sm, md, lg, xl };
    radii: { sm, md, lg, full };
  }
}
```

This means `theme` is fully typed in any `styled` component or `css` interpolation.

## Usage

### Custom Styled Component

```typescript
import { styled, css } from '@putnami/ui';

const MyCard = styled.div`
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: ${({ theme }) => theme.radii.lg};
  padding: ${({ theme }) => theme.space.lg};

  ${({ theme }) => css`
    @media (min-width: ${theme.breakpoints.md}px) {
      padding: ${theme.space.xl};
    }
  `}
`;
```

Read colors from the `--color-*` custom properties, not from `theme.colors.*`.
Emotion serialises the declaration into the class name once, during SSR, where
the palette is always light — a baked literal never flips when the user switches
to dark mode. Mode-independent tokens (spacing, radii, type ramp, breakpoints)
are safe to read from the theme.

### Extending Box

Build on the layout primitives:

```typescript
import { styled } from '@putnami/ui';
import { Box } from '@putnami/ui';

const Sidebar = styled(Box)`
  position: sticky;
  top: 0;
  height: 100vh;
  border-right: 1px solid var(--color-border);
`;
```

### Keyframe Animations

```typescript
import { keyframes, styled } from '@putnami/ui';

const pulse = keyframes`
  0%, 100% { opacity: 1; }
  50% { opacity: 0.5; }
`;

const PulsingDot = styled.span`
  animation: ${pulse} 2s ease-in-out infinite;
`;
```
