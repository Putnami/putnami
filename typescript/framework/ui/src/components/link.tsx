import { Link as RouterLink } from '@putnami/web';
import { styled } from '../emotion';

/**
 * Styled anchor element using the framework router, with hover color transition.
 *
 * Colors come from the custom properties `GlobalStyles` emits, so they follow the
 * color mode: Emotion bakes the class once during SSR, where the palette is always
 * light, and a literal would never flip to dark.
 */
export const Link = styled(RouterLink)`
  text-decoration: none;
  color: var(--color-text);
  &:hover {
    color: var(--color-text-muted);
  }
`;
