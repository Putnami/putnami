import type { ComponentProps, ReactNode } from 'react';
import { css, styled } from '../emotion';
import { Box, type BoxProps } from '../layout';

type CardVariant = 'elevated' | 'outline' | 'filled';

/** Props for the Card component, a content container with elevated, outline, and filled variants. */
export interface CardProps extends BoxProps {
  variant?: CardVariant;
  children?: ReactNode;
}

/** Props for the CardHeader section. */
export interface CardHeaderProps extends BoxProps {
  children?: ReactNode;
}

/** Props for the CardBody section. */
export interface CardBodyProps extends BoxProps {
  children?: ReactNode;
}

/** Props for the CardFooter section. */
export interface CardFooterProps extends BoxProps {
  children?: ReactNode;
}

const StyledCard = styled(Box)<{ $variant: CardVariant }>`
  display: flex;
  flex-direction: column;
  background: var(--color-bg);
  border-radius: var(--radius-lg);
  overflow: hidden;

  /* Variant: elevated */
  ${({ $variant }) =>
    $variant === 'elevated' &&
    css`
      box-shadow: var(--shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
    `}

  /* Variant: outline */
  ${({ $variant }) =>
    $variant === 'outline' &&
    css`
      border: 1px solid var(--color-border);
    `}

  /* Variant: filled */
  ${({ $variant }) =>
    $variant === 'filled' &&
    css`
      background: var(--color-surface);
    `}
`;

const StyledCardHeader = styled(Box)`
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-lg);
  border-bottom: 1px solid var(--color-border);
`;

const StyledCardBody = styled(Box)`
  flex: 1;
  padding: var(--space-lg);
`;

const StyledCardFooter = styled(Box)`
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: var(--space-sm);
  padding: var(--space-lg);
  border-top: 1px solid var(--color-border);
`;

/** Renders a card container with configurable visual variant. */
export function Card({ variant = 'elevated', children, ...props }: CardProps & ComponentProps<typeof Box>) {
  return (
    <StyledCard $variant={variant} {...props}>
      {children}
    </StyledCard>
  );
}

/** Renders the header section of a Card with a bottom border. */
export function CardHeader({ children, ...props }: CardHeaderProps & ComponentProps<typeof Box>) {
  return <StyledCardHeader {...props}>{children}</StyledCardHeader>;
}

/** Renders the main body section of a Card. */
export function CardBody({ children, ...props }: CardBodyProps & ComponentProps<typeof Box>) {
  return <StyledCardBody {...props}>{children}</StyledCardBody>;
}

/** Renders the footer section of a Card with a top border. */
export function CardFooter({ children, ...props }: CardFooterProps & ComponentProps<typeof Box>) {
  return <StyledCardFooter {...props}>{children}</StyledCardFooter>;
}
