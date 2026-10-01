import { styled } from '../emotion';

/**
 * Full-viewport page wrapper with theme background, text color, and default padding.
 *
 * Colors come from the custom properties `GlobalStyles` emits, so they follow the
 * color mode: Emotion bakes the class once during SSR, where the palette is always
 * light, and a literal would never flip to dark.
 */
export const Page = styled.div`
  min-height: 100vh;
  background: var(--color-bg);
  color: var(--color-text);
  padding: ${({ theme }) => theme.space.md};
`;
