'use client';

import {
  type ComponentPropsWithoutRef,
  forwardRef,
  type MouseEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useRef,
} from 'react';
import { createPortal } from 'react-dom';
import { css, keyframes, styled } from '../emotion';
import { useFocusTrap } from '../hooks/use-focus-trap';

type ModalSize = 'sm' | 'md' | 'lg' | 'xl' | 'full';

/** Props for the Modal component, a centered overlay dialog with backdrop. */
export interface ModalProps {
  isOpen: boolean;
  onClose: () => void;
  size?: ModalSize;
  closeOnOverlayClick?: boolean;
  closeOnEsc?: boolean;
  children?: ReactNode;
  className?: string;
}

/** Props for the ModalHeader section. */
export interface ModalHeaderProps extends ComponentPropsWithoutRef<'div'> {
  children?: ReactNode;
}

/** Props for the ModalBody section. */
export interface ModalBodyProps extends ComponentPropsWithoutRef<'div'> {
  children?: ReactNode;
}

/** Props for the ModalFooter section. */
export interface ModalFooterProps extends ComponentPropsWithoutRef<'div'> {
  children?: ReactNode;
}

/** Props for the ModalCloseButton control. */
export interface ModalCloseButtonProps extends ComponentPropsWithoutRef<'button'> {
  onClose: () => void;
}

const sizeStyles: Record<ModalSize, string> = {
  sm: '400px',
  md: '500px',
  lg: '700px',
  xl: '900px',
  full: 'calc(100vw - 32px)',
};

const fadeIn = keyframes`
  from {
    opacity: 0;
  }
  to {
    opacity: 1;
  }
`;

const slideIn = keyframes`
  from {
    opacity: 0;
    transform: translate(-50%, -48%) scale(0.96);
  }
  to {
    opacity: 1;
    transform: translate(-50%, -50%) scale(1);
  }
`;

const Overlay = styled.div`
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  z-index: 1000;
  animation: ${fadeIn} 0.2s ease-out;
`;

const ModalContainer = styled.div<{ $size: ModalSize }>`
  position: fixed;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  z-index: 1001;
  width: 90%;
  max-height: calc(100vh - 64px);
  background: var(--color-bg);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-xl, 0 25px 50px -12px rgba(0, 0, 0, 0.25));
  display: flex;
  flex-direction: column;
  animation: ${slideIn} 0.2s ease-out;

  ${({ $size }) => css`
    max-width: ${sizeStyles[$size]};
  `}

  ${({ $size }) =>
    $size === 'full' &&
    css`
      max-height: calc(100vh - 32px);
      border-radius: var(--radius-md);
    `}
`;

const StyledModalHeader = styled.div`
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-lg);
  border-bottom: 1px solid var(--color-border);
  font-family: var(--font-sans);
  font-weight: 600;
  font-size: 1.125rem;
  color: var(--color-text);
`;

const StyledModalBody = styled.div`
  flex: 1;
  padding: var(--space-lg);
  overflow-y: auto;
  font-family: var(--font-sans);
  color: var(--color-text);
`;

const StyledModalFooter = styled.div`
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: var(--space-sm);
  padding: var(--space-lg);
  border-top: 1px solid var(--color-border);
`;

const CloseButton = styled.button`
  display: flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  padding: 0;
  background: transparent;
  border: none;
  border-radius: var(--radius-md);
  cursor: pointer;
  color: var(--color-text-muted);
  transition: all var(--transition-fast);

  &:hover {
    background: var(--color-surface-hover);
    color: var(--color-text);
  }

  &:focus-visible {
    outline: 2px solid var(--color-primary);
    outline-offset: 2px;
  }

  svg {
    width: 20px;
    height: 20px;
  }
`;

const closeIcon = (
  <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Close</title>
    <line x1='18' y1='6' x2='6' y2='18' />
    <line x1='6' y1='6' x2='18' y2='18' />
  </svg>
);

/** Renders a centered modal dialog with overlay backdrop, keyboard and click dismissal. */
export function Modal({
  isOpen,
  onClose,
  size = 'md',
  closeOnOverlayClick = true,
  closeOnEsc = true,
  children,
  className,
}: ModalProps) {
  const containerRef = useRef<HTMLDivElement>(null);

  const handleEsc = useCallback(
    (e: KeyboardEvent) => {
      if (closeOnEsc && e.key === 'Escape') {
        onClose();
      }
    },
    [closeOnEsc, onClose],
  );

  const handleOverlayClick = (e: MouseEvent) => {
    if (closeOnOverlayClick && e.target === e.currentTarget) {
      onClose();
    }
  };

  useEffect(() => {
    if (isOpen) {
      document.addEventListener('keydown', handleEsc);
      document.body.style.overflow = 'hidden';

      return () => {
        document.removeEventListener('keydown', handleEsc);
        document.body.style.overflow = '';
      };
    }
    return undefined;
  }, [isOpen, handleEsc]);

  // Honour the aria-modal="true" contract: contain Tab focus and restore it on close.
  useFocusTrap(containerRef, isOpen);

  if (!isOpen) return null;

  if (typeof document === 'undefined') return null;

  return createPortal(
    <>
      <Overlay onClick={handleOverlayClick} />
      <ModalContainer ref={containerRef} $size={size} className={className} role='dialog' aria-modal='true'>
        {children}
      </ModalContainer>
    </>,
    document.body,
  );
}

/** Renders the header section of a Modal with a bottom border. */
export const ModalHeader = forwardRef<HTMLDivElement, ModalHeaderProps>(function ModalHeader(
  { children, ...rest },
  ref,
) {
  return (
    <StyledModalHeader ref={ref} {...rest}>
      {children}
    </StyledModalHeader>
  );
});

/** Renders the scrollable body section of a Modal. */
export const ModalBody = forwardRef<HTMLDivElement, ModalBodyProps>(function ModalBody({ children, ...rest }, ref) {
  return (
    <StyledModalBody ref={ref} {...rest}>
      {children}
    </StyledModalBody>
  );
});

/** Renders the footer section of a Modal with a top border. */
export const ModalFooter = forwardRef<HTMLDivElement, ModalFooterProps>(function ModalFooter(
  { children, ...rest },
  ref,
) {
  return (
    <StyledModalFooter ref={ref} {...rest}>
      {children}
    </StyledModalFooter>
  );
});

/** Renders a close button for the Modal. */
export const ModalCloseButton = forwardRef<HTMLButtonElement, ModalCloseButtonProps>(function ModalCloseButton(
  { onClose, onClick, ...rest },
  ref,
) {
  return (
    <CloseButton
      ref={ref}
      type='button'
      aria-label='Close modal'
      {...rest}
      onClick={(event) => {
        onClick?.(event);
        if (!event.defaultPrevented) {
          onClose();
        }
      }}
    >
      {closeIcon}
    </CloseButton>
  );
});
