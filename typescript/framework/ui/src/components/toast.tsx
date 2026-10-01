'use client';

import { createContext, type ReactNode, useCallback, useContext, useState } from 'react';
import { css, keyframes, styled } from '../emotion';
import type { Theme } from '../theme/theme';

type ToastStatus = 'info' | 'success' | 'warning' | 'error';
type ToastPosition = 'top' | 'top-right' | 'top-left' | 'bottom' | 'bottom-right' | 'bottom-left';

/** Configuration for creating a toast notification with status styling and auto-dismiss. */
export interface ToastOptions {
  title?: string;
  description?: string;
  status?: ToastStatus;
  duration?: number;
  isClosable?: boolean;
  position?: ToastPosition;
}

interface Toast extends ToastOptions {
  id: string;
}

interface ToastContextValue {
  toast: (options: ToastOptions) => string;
  closeToast: (id: string) => void;
  closeAll: () => void;
}

const ToastContext = createContext<ToastContextValue | null>(null);

/** Returns the toast context for creating, closing, and managing toast notifications. */
export function useToast(): ToastContextValue {
  const context = useContext(ToastContext);
  if (!context) {
    throw new Error('useToast must be used within a ToastProvider');
  }
  return context;
}

const slideIn = keyframes`
  from {
    transform: translateX(100%);
    opacity: 0;
  }
  to {
    transform: translateX(0);
    opacity: 1;
  }
`;

const slideOut = keyframes`
  from {
    transform: translateX(0);
    opacity: 1;
  }
  to {
    transform: translateX(100%);
    opacity: 0;
  }
`;

const getStatusColor = (theme: Theme, status: ToastStatus) => theme.colors[status];

const ToastContainer = styled.div<{ $position: ToastPosition }>`
  position: fixed;
  z-index: 1500;
  display: flex;
  flex-direction: column;
  gap: var(--space-sm);
  pointer-events: none;
  max-width: 400px;
  width: 100%;
  padding: var(--space-md);

  ${({ $position }) => {
    switch ($position) {
      case 'top':
        return css`
          top: 0;
          left: 50%;
          transform: translateX(-50%);
          align-items: center;
        `;
      case 'top-right':
        return css`
          top: 0;
          right: 0;
          align-items: flex-end;
        `;
      case 'top-left':
        return css`
          top: 0;
          left: 0;
          align-items: flex-start;
        `;
      case 'bottom':
        return css`
          bottom: 0;
          left: 50%;
          transform: translateX(-50%);
          align-items: center;
          flex-direction: column-reverse;
        `;
      case 'bottom-right':
        return css`
          bottom: 0;
          right: 0;
          align-items: flex-end;
          flex-direction: column-reverse;
        `;
      case 'bottom-left':
        return css`
          bottom: 0;
          left: 0;
          align-items: flex-start;
          flex-direction: column-reverse;
        `;
    }
  }}
`;

const ToastItem = styled.div<{ $status: ToastStatus; $isExiting?: boolean }>`
  display: flex;
  align-items: flex-start;
  gap: var(--space-md);
  padding: var(--space-md) var(--space-lg);
  background: var(--color-bg);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-lg, 0 10px 15px -3px rgba(0, 0, 0, 0.1));
  pointer-events: auto;
  max-width: 100%;
  animation: ${slideIn} 0.3s ease-out;

  ${({ theme, $status }) => css`
    border-left: 4px solid ${getStatusColor(theme, $status).main};
  `}

  ${({ $isExiting }) =>
    $isExiting &&
    css`
      animation: ${slideOut} 0.2s ease-in forwards;
    `}
`;

const IconWrapper = styled.span<{ $status: ToastStatus }>`
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  width: 20px;
  height: 20px;

  ${({ theme, $status }) => css`
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
  font-size: 0.9rem;
  color: var(--color-text);
`;

const Description = styled.div`
  font-size: 0.85rem;
  color: var(--color-text-muted);
  margin-top: var(--space-xs);
`;

const CloseButton = styled.button`
  display: flex;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 20px;
  padding: 0;
  background: transparent;
  border: none;
  border-radius: var(--radius-sm);
  cursor: pointer;
  color: var(--color-text-muted);
  transition: all var(--transition-fast);

  &:hover {
    color: var(--color-text);
    background: var(--color-surface-hover);
  }

  svg {
    width: 14px;
    height: 14px;
  }
`;

const statusIcons: Record<ToastStatus, ReactNode> = {
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

let toastId = 0;
const generateId = () => `toast-${++toastId}`;

interface ToastProviderProps {
  children: ReactNode;
  defaultPosition?: ToastPosition;
}

/** Provides toast notification context and renders active toasts grouped by screen position. */
export function ToastProvider({ children, defaultPosition = 'top-right' }: ToastProviderProps) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const [exitingIds, setExitingIds] = useState<Set<string>>(new Set());

  const closeToast = useCallback((id: string) => {
    setExitingIds((prev) => new Set(prev).add(id));
    setTimeout(() => {
      setToasts((prev) => prev.filter((t) => t.id !== id));
      setExitingIds((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
    }, 200);
  }, []);

  const toast = useCallback(
    (options: ToastOptions) => {
      const id = generateId();
      const newToast: Toast = {
        id,
        status: 'info',
        duration: 5000,
        isClosable: true,
        position: defaultPosition,
        ...options,
      };

      setToasts((prev) => [...prev, newToast]);

      if (newToast.duration && newToast.duration > 0) {
        setTimeout(() => {
          closeToast(id);
        }, newToast.duration);
      }

      return id;
    },
    [defaultPosition, closeToast],
  );

  const closeAll = useCallback(() => {
    for (const t of toasts) {
      closeToast(t.id);
    }
  }, [toasts, closeToast]);

  // Group toasts by position
  const toastsByPosition = toasts.reduce(
    (acc, t) => {
      const pos = t.position || defaultPosition;
      if (!acc[pos]) acc[pos] = [];
      acc[pos].push(t);
      return acc;
    },
    {} as Record<ToastPosition, Toast[]>,
  );

  return (
    <ToastContext.Provider value={{ toast, closeToast, closeAll }}>
      {children}
      {Object.entries(toastsByPosition).map(([position, positionToasts]) => (
        <ToastContainer key={position} $position={position as ToastPosition}>
          {positionToasts.map((t) => (
            <ToastItem key={t.id} $status={t.status || 'info'} $isExiting={exitingIds.has(t.id)} role='alert'>
              <IconWrapper $status={t.status || 'info'}>{statusIcons[t.status || 'info']}</IconWrapper>
              <Content>
                {!!t.title && <Title>{t.title}</Title>}
                {!!t.description && <Description>{t.description}</Description>}
              </Content>
              {t.isClosable ? (
                <CloseButton onClick={() => closeToast(t.id)} aria-label='Close' type='button'>
                  {closeIcon}
                </CloseButton>
              ) : null}
            </ToastItem>
          ))}
        </ToastContainer>
      ))}
    </ToastContext.Provider>
  );
}
