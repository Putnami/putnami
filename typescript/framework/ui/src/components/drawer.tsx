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

type DrawerPlacement = 'left' | 'right' | 'top' | 'bottom';
type DrawerSize = 'xs' | 'sm' | 'md' | 'lg' | 'xl' | 'full';

/** Props for the Drawer component, a slide-in panel from a screen edge. */
export interface DrawerProps {
  isOpen: boolean;
  onClose: () => void;
  placement?: DrawerPlacement;
  size?: DrawerSize;
  closeOnOverlayClick?: boolean;
  closeOnEsc?: boolean;
  children?: ReactNode;
  className?: string;
}

/** Props for the DrawerHeader section. */
export interface DrawerHeaderProps extends ComponentPropsWithoutRef<'div'> {
  children?: ReactNode;
}

/** Props for the DrawerBody section. */
export interface DrawerBodyProps extends ComponentPropsWithoutRef<'div'> {
  children?: ReactNode;
}

/** Props for the DrawerFooter section. */
export interface DrawerFooterProps extends ComponentPropsWithoutRef<'div'> {
  children?: ReactNode;
}

/** Props for the DrawerCloseButton control. */
export interface DrawerCloseButtonProps extends ComponentPropsWithoutRef<'button'> {
  onClose: () => void;
}

const horizontalSizes: Record<DrawerSize, string> = {
  xs: '256px',
  sm: '320px',
  md: '400px',
  lg: '512px',
  xl: '640px',
  full: '100vw',
};

const verticalSizes: Record<DrawerSize, string> = {
  xs: '200px',
  sm: '300px',
  md: '400px',
  lg: '500px',
  xl: '600px',
  full: '100vh',
};

const fadeIn = keyframes`
  from { opacity: 0; }
  to { opacity: 1; }
`;

const slideFromLeft = keyframes`
  from { transform: translateX(-100%); }
  to { transform: translateX(0); }
`;

const slideFromRight = keyframes`
  from { transform: translateX(100%); }
  to { transform: translateX(0); }
`;

const slideFromTop = keyframes`
  from { transform: translateY(-100%); }
  to { transform: translateY(0); }
`;

const slideFromBottom = keyframes`
  from { transform: translateY(100%); }
  to { transform: translateY(0); }
`;

const animations: Record<DrawerPlacement, ReturnType<typeof keyframes>> = {
  left: slideFromLeft,
  right: slideFromRight,
  top: slideFromTop,
  bottom: slideFromBottom,
};

const Overlay = styled.div`
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  z-index: 1000;
  animation: ${fadeIn} 0.2s ease-out;
`;

const DrawerContainer = styled.div<{
  $placement: DrawerPlacement;
  $size: DrawerSize;
}>`
  position: fixed;
  z-index: 1001;
  background: var(--color-bg);
  display: flex;
  flex-direction: column;
  box-shadow: var(--shadow-xl, 0 25px 50px -12px rgba(0, 0, 0, 0.25));

  ${({ $placement, $size }) => {
    const isHorizontal = $placement === 'left' || $placement === 'right';
    const sizeValue = isHorizontal ? horizontalSizes[$size] : verticalSizes[$size];

    if ($placement === 'left') {
      return css`
        top: 0;
        left: 0;
        bottom: 0;
        width: ${sizeValue};
        max-width: 100vw;
        animation: ${animations.left} 0.3s ease-out;
      `;
    }
    if ($placement === 'right') {
      return css`
        top: 0;
        right: 0;
        bottom: 0;
        width: ${sizeValue};
        max-width: 100vw;
        animation: ${animations.right} 0.3s ease-out;
      `;
    }
    if ($placement === 'top') {
      return css`
        top: 0;
        left: 0;
        right: 0;
        height: ${sizeValue};
        max-height: 100vh;
        animation: ${animations.top} 0.3s ease-out;
      `;
    }
    // $placement === 'bottom'
    return css`
      bottom: 0;
      left: 0;
      right: 0;
      height: ${sizeValue};
      max-height: 100vh;
      animation: ${animations.bottom} 0.3s ease-out;
    `;
  }}
`;

const StyledDrawerHeader = styled.div`
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

const StyledDrawerBody = styled.div`
  flex: 1;
  padding: var(--space-lg);
  overflow-y: auto;
  font-family: var(--font-sans);
  color: var(--color-text);
`;

const StyledDrawerFooter = styled.div`
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

/** Renders a slide-in panel from a screen edge with overlay backdrop and keyboard/click dismissal. */
export function Drawer({
  isOpen,
  onClose,
  placement = 'right',
  size = 'md',
  closeOnOverlayClick = true,
  closeOnEsc = true,
  children,
  className,
}: DrawerProps) {
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
      <DrawerContainer
        ref={containerRef}
        $placement={placement}
        $size={size}
        className={className}
        role='dialog'
        aria-modal='true'
      >
        {children}
      </DrawerContainer>
    </>,
    document.body,
  );
}

/** Renders the header section of a Drawer with a bottom border. */
export const DrawerHeader = forwardRef<HTMLDivElement, DrawerHeaderProps>(function DrawerHeader(
  { children, ...rest },
  ref,
) {
  return (
    <StyledDrawerHeader ref={ref} {...rest}>
      {children}
    </StyledDrawerHeader>
  );
});

/** Renders the scrollable body section of a Drawer. */
export const DrawerBody = forwardRef<HTMLDivElement, DrawerBodyProps>(function DrawerBody({ children, ...rest }, ref) {
  return (
    <StyledDrawerBody ref={ref} {...rest}>
      {children}
    </StyledDrawerBody>
  );
});

/** Renders the footer section of a Drawer with a top border. */
export const DrawerFooter = forwardRef<HTMLDivElement, DrawerFooterProps>(function DrawerFooter(
  { children, ...rest },
  ref,
) {
  return (
    <StyledDrawerFooter ref={ref} {...rest}>
      {children}
    </StyledDrawerFooter>
  );
});

/** Renders a close button for the Drawer. */
export const DrawerCloseButton = forwardRef<HTMLButtonElement, DrawerCloseButtonProps>(function DrawerCloseButton(
  { onClose, onClick, ...rest },
  ref,
) {
  return (
    <CloseButton
      ref={ref}
      type='button'
      aria-label='Close drawer'
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
