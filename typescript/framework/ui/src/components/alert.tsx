'use client';

import type { ComponentPropsWithoutRef, ReactNode } from 'react';
import { forwardRef } from 'react';
import { css, styled } from '../emotion';
import type { Theme } from '../theme/theme';

type AlertStatus = 'info' | 'success' | 'warning' | 'error';
type AlertVariant = 'subtle' | 'solid' | 'left-accent' | 'top-accent';

/** Props for the Alert component, a contextual feedback message with status-based styling. */
export interface AlertProps extends Omit<ComponentPropsWithoutRef<'div'>, 'title'> {
  status?: AlertStatus;
  variant?: AlertVariant;
  title?: ReactNode;
  icon?: ReactNode;
  children?: ReactNode;
  onClose?: () => void;
}

const getStatusColor = (theme: Theme, status: AlertStatus) => theme.colors[status];

const StyledAlert = styled.div<{
  $status: AlertStatus;
  $variant: AlertVariant;
}>`
  display: flex;
  align-items: flex-start;
  gap: var(--space-md);
  padding: var(--space-md) var(--space-lg);
  border-radius: var(--radius-md);
  font-family: var(--font-sans);
  position: relative;

  /* Variant: subtle */
  ${({ theme, $status, $variant }) =>
    $variant === 'subtle' &&
    css`
      background: ${getStatusColor(theme, $status)[50]};
      color: ${getStatusColor(theme, $status)[800]};
    `}

  /* Variant: solid */
  ${({ theme, $status, $variant }) =>
    $variant === 'solid' &&
    css`
      background: ${getStatusColor(theme, $status).main};
      color: ${getStatusColor(theme, $status).contrastText};
    `}

  /* Variant: left-accent */
  ${({ theme, $status, $variant }) =>
    $variant === 'left-accent' &&
    css`
      background: ${getStatusColor(theme, $status)[50]};
      color: ${getStatusColor(theme, $status)[800]};
      border-left: 4px solid ${getStatusColor(theme, $status).main};
      border-radius: 0 var(--radius-md) var(--radius-md) 0;
    `}

  /* Variant: top-accent */
  ${({ theme, $status, $variant }) =>
    $variant === 'top-accent' &&
    css`
      background: ${getStatusColor(theme, $status)[50]};
      color: ${getStatusColor(theme, $status)[800]};
      border-top: 4px solid ${getStatusColor(theme, $status).main};
      border-radius: 0 0 var(--radius-md) var(--radius-md);
    `}
`;

const IconWrapper = styled.span<{ $status: AlertStatus; $variant: AlertVariant }>`
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  width: 20px;
  height: 20px;
  margin-top: 2px;

  ${({ theme, $status, $variant }) =>
    $variant === 'solid'
      ? css`
          color: ${getStatusColor(theme, $status).contrastText};
        `
      : css`
          color: ${getStatusColor(theme, $status).main};
        `}

  svg {
    width: 100%;
    height: 100%;
  }
`;

const Content = styled.div`
  flex: 1;
  min-width: 0;
`;

const Title = styled.div`
  font-weight: 600;
  margin-bottom: var(--space-xs);
`;

const Description = styled.div`
  font-size: 0.9rem;
  line-height: 1.5;
`;

const CloseButton = styled.button<{ $status: AlertStatus; $variant: AlertVariant }>`
  position: absolute;
  top: var(--space-sm);
  right: var(--space-sm);
  display: flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  padding: 0;
  background: transparent;
  border: none;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: all var(--transition-fast);

  ${({ theme, $status, $variant }) =>
    $variant === 'solid'
      ? css`
          color: ${getStatusColor(theme, $status).contrastText};
          opacity: 0.8;

          &:hover {
            opacity: 1;
            background: rgba(255, 255, 255, 0.1);
          }
        `
      : css`
          color: ${getStatusColor(theme, $status)[600]};

          &:hover {
            background: ${getStatusColor(theme, $status)[100]};
          }
        `}

  svg {
    width: 14px;
    height: 14px;
  }
`;

// Default icons for each status
const defaultIcons: Record<AlertStatus, ReactNode> = {
  info: (
    <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
      <title>Info</title>
      <circle cx='12' cy='12' r='10' />
      <line x1='12' y1='16' x2='12' y2='12' />
      <line x1='12' y1='8' x2='12.01' y2='8' />
    </svg>
  ),
  success: (
    <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
      <title>Success</title>
      <path d='M22 11.08V12a10 10 0 1 1-5.93-9.14' />
      <polyline points='22 4 12 14.01 9 11.01' />
    </svg>
  ),
  warning: (
    <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
      <title>Warning</title>
      <path d='M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z' />
      <line x1='12' y1='9' x2='12' y2='13' />
      <line x1='12' y1='17' x2='12.01' y2='17' />
    </svg>
  ),
  error: (
    <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
      <title>Error</title>
      <circle cx='12' cy='12' r='10' />
      <line x1='15' y1='9' x2='9' y2='15' />
      <line x1='9' y1='9' x2='15' y2='15' />
    </svg>
  ),
};

const closeIcon = (
  <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Close</title>
    <line x1='18' y1='6' x2='6' y2='18' />
    <line x1='6' y1='6' x2='18' y2='18' />
  </svg>
);

/** Renders a contextual alert message with status icon, title, description, and optional close button. */
export const Alert = forwardRef<HTMLDivElement, AlertProps>(function Alert(
  { status = 'info', variant = 'subtle', title, icon, children, onClose, ...rest },
  ref,
) {
  const displayIcon = icon === undefined ? defaultIcons[status] : icon;

  return (
    <StyledAlert ref={ref} $status={status} $variant={variant} role='alert' {...rest}>
      {!!displayIcon && (
        <IconWrapper $status={status} $variant={variant}>
          {displayIcon}
        </IconWrapper>
      )}
      <Content>
        {!!title && <Title>{title}</Title>}
        {!!children && <Description>{children}</Description>}
      </Content>
      {!!onClose && (
        <CloseButton $status={status} $variant={variant} onClick={onClose} aria-label='Close alert' type='button'>
          {closeIcon}
        </CloseButton>
      )}
    </StyledAlert>
  );
});
