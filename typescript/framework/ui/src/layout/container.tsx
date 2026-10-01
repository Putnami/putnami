import { styled } from '../emotion';
import { Box, type BoxProps } from './box';

/** Centered content container with responsive horizontal padding. Defaults to the theme's xl breakpoint as max-width; pass maxWidth to override. */
export const Container = styled(Box)<BoxProps>`
  width: 100%;
  margin-left: auto;
  margin-right: auto;
  padding-left: var(--space-lg);
  padding-right: var(--space-lg);

  @media (min-width: 768px) {
    padding-left: var(--space-2xl);
    padding-right: var(--space-2xl);
  }

  ${({ theme, maxWidth }) => (maxWidth ? '' : `max-width: ${theme.breakpoints.xl}px;`)}
`;
