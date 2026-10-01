'use client';

import {
  cloneElement,
  type ComponentPropsWithoutRef,
  forwardRef,
  type ReactElement,
  type MouseEvent as ReactMouseEvent,
  type ReactNode,
  type Ref,
  useCallback,
  useEffect,
  useRef,
  useState,
} from 'react';
import { createPortal } from 'react-dom';
import { css, keyframes, styled } from '../emotion';
import { isOutsideClick } from '../hooks/use-dismiss';

type PopoverPlacement = 'top' | 'bottom' | 'left' | 'right';
type PopoverTrigger = 'click' | 'hover';

type TriggerChildProps = {
  onClick?: (event: ReactMouseEvent<HTMLElement>) => void;
  onMouseEnter?: (event: ReactMouseEvent<HTMLElement>) => void;
  onMouseLeave?: (event: ReactMouseEvent<HTMLElement>) => void;
  ref?: Ref<HTMLElement>;
};

/** Props for the Popover component, a floating content panel anchored to a trigger element. */
export interface PopoverProps {
  placement?: PopoverPlacement;
  trigger?: PopoverTrigger;
  offset?: number;
  isOpen?: boolean;
  onOpen?: () => void;
  onClose?: () => void;
  closeOnBlur?: boolean;
  children: ReactElement<TriggerChildProps>;
  content: ReactNode;
  className?: string;
}

/** Props for the PopoverHeader section. */
export interface PopoverHeaderProps extends ComponentPropsWithoutRef<'div'> {
  children?: ReactNode;
}

/** Props for the PopoverBody section. */
export interface PopoverBodyProps extends ComponentPropsWithoutRef<'div'> {
  children?: ReactNode;
}

/** Props for the PopoverCloseButton control. */
export interface PopoverCloseButtonProps extends ComponentPropsWithoutRef<'button'> {
  onClose: () => void;
}

const fadeIn = keyframes`
  from {
    opacity: 0;
    transform: scale(0.95);
  }
  to {
    opacity: 1;
    transform: scale(1);
  }
`;

const PopoverContainer = styled.div<{ $placement: PopoverPlacement }>`
  position: fixed;
  z-index: 1000;
  background: var(--color-bg);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-lg, 0 10px 15px -3px rgba(0, 0, 0, 0.1));
  min-width: 200px;
  max-width: 350px;
  animation: ${fadeIn} 0.15s ease-out;

  /* Arrow */
  &::before {
    content: '';
    position: absolute;
    width: 12px;
    height: 12px;
    background: var(--color-bg);
    border: 1px solid var(--color-border);
    transform: rotate(45deg);

    ${({ $placement }) => {
      switch ($placement) {
        case 'top':
          return css`
            bottom: -7px;
            left: 50%;
            margin-left: -6px;
            border-top: none;
            border-left: none;
          `;
        case 'bottom':
          return css`
            top: -7px;
            left: 50%;
            margin-left: -6px;
            border-bottom: none;
            border-right: none;
          `;
        case 'left':
          return css`
            right: -7px;
            top: 50%;
            margin-top: -6px;
            border-bottom: none;
            border-left: none;
          `;
        case 'right':
          return css`
            left: -7px;
            top: 50%;
            margin-top: -6px;
            border-top: none;
            border-right: none;
          `;
      }
    }}
  }
`;

const StyledPopoverHeader = styled.div`
  padding: var(--space-md) var(--space-lg);
  border-bottom: 1px solid var(--color-border);
  font-family: var(--font-sans);
  font-weight: 600;
  font-size: 0.95rem;
  color: var(--color-text);
`;

const StyledPopoverBody = styled.div`
  padding: var(--space-md) var(--space-lg);
  font-family: var(--font-sans);
  font-size: 0.9rem;
  color: var(--color-text);
`;

const CloseButton = styled.button`
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
  color: var(--color-text-muted);
  transition: all var(--transition-fast);

  &:hover {
    background: var(--color-surface-hover);
    color: var(--color-text);
  }

  svg {
    width: 14px;
    height: 14px;
  }
`;

const closeIcon = (
  <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Close</title>
    <line x1='18' y1='6' x2='6' y2='18' />
    <line x1='6' y1='6' x2='18' y2='18' />
  </svg>
);

/** Renders a floating content panel anchored to a trigger element, activated by click or hover. */
export function Popover({
  placement = 'bottom',
  trigger = 'click',
  offset = 12,
  isOpen: controlledIsOpen,
  onOpen,
  onClose,
  closeOnBlur = true,
  children,
  content,
  className,
}: PopoverProps) {
  const [internalIsOpen, setInternalIsOpen] = useState(false);
  const isOpen = controlledIsOpen !== undefined ? controlledIsOpen : internalIsOpen;

  const [position, setPosition] = useState({ top: 0, left: 0 });
  const triggerRef = useRef<HTMLElement>(null);
  const popoverRef = useRef<HTMLDivElement>(null);

  const open = useCallback(() => {
    if (controlledIsOpen === undefined) {
      setInternalIsOpen(true);
    }
    onOpen?.();
  }, [controlledIsOpen, onOpen]);

  const close = useCallback(() => {
    if (controlledIsOpen === undefined) {
      setInternalIsOpen(false);
    }
    onClose?.();
  }, [controlledIsOpen, onClose]);

  const toggle = useCallback(() => {
    if (isOpen) {
      close();
    } else {
      open();
    }
  }, [isOpen, open, close]);

  const calculatePosition = useCallback(() => {
    if (!triggerRef.current || !popoverRef.current) return;

    const triggerRect = triggerRef.current.getBoundingClientRect();
    const popoverRect = popoverRef.current.getBoundingClientRect();

    let top = 0;
    let left = 0;

    switch (placement) {
      case 'top':
        top = triggerRect.top - popoverRect.height - offset;
        left = triggerRect.left + triggerRect.width / 2 - popoverRect.width / 2;
        break;
      case 'bottom':
        top = triggerRect.bottom + offset;
        left = triggerRect.left + triggerRect.width / 2 - popoverRect.width / 2;
        break;
      case 'left':
        top = triggerRect.top + triggerRect.height / 2 - popoverRect.height / 2;
        left = triggerRect.left - popoverRect.width - offset;
        break;
      case 'right':
        top = triggerRect.top + triggerRect.height / 2 - popoverRect.height / 2;
        left = triggerRect.right + offset;
        break;
    }

    // Viewport boundary checks
    const padding = 8;
    if (left < padding) left = padding;
    if (left + popoverRect.width > window.innerWidth - padding) {
      left = window.innerWidth - popoverRect.width - padding;
    }
    if (top < padding) top = padding;
    if (top + popoverRect.height > window.innerHeight - padding) {
      top = window.innerHeight - popoverRect.height - padding;
    }

    setPosition({ top, left });
  }, [placement, offset]);

  useEffect(() => {
    if (isOpen) {
      calculatePosition();
      window.addEventListener('scroll', calculatePosition, true);
      window.addEventListener('resize', calculatePosition);

      return () => {
        window.removeEventListener('scroll', calculatePosition, true);
        window.removeEventListener('resize', calculatePosition);
      };
    }
    return undefined;
  }, [isOpen, calculatePosition]);

  useEffect(() => {
    if (isOpen && closeOnBlur) {
      const handleClickOutside = (e: MouseEvent) => {
        if (isOutsideClick(e.target, popoverRef.current, triggerRef.current)) {
          close();
        }
      };

      document.addEventListener('mousedown', handleClickOutside);
      return () => document.removeEventListener('mousedown', handleClickOutside);
    }
    return undefined;
  }, [isOpen, closeOnBlur, close]);

  const triggerProps =
    trigger === 'click'
      ? {
          onClick: (event: ReactMouseEvent<HTMLElement>) => {
            toggle();
            children.props.onClick?.(event);
          },
        }
      : {
          onMouseEnter: (event: ReactMouseEvent<HTMLElement>) => {
            open();
            children.props.onMouseEnter?.(event);
          },
          onMouseLeave: (event: ReactMouseEvent<HTMLElement>) => {
            close();
            children.props.onMouseLeave?.(event);
          },
        };

  const triggerElement = cloneElement(children, {
    ref: triggerRef as Ref<HTMLElement>,
    ...triggerProps,
  });

  return (
    <>
      {triggerElement}
      {!!isOpen &&
        typeof document !== 'undefined' &&
        createPortal(
          <PopoverContainer
            ref={popoverRef}
            $placement={placement}
            className={className}
            style={{ top: position.top, left: position.left }}
            onMouseEnter={trigger === 'hover' ? open : undefined}
            onMouseLeave={trigger === 'hover' ? close : undefined}
          >
            {content}
          </PopoverContainer>,
          document.body,
        )}
    </>
  );
}

/** Renders the header section of a Popover. */
export const PopoverHeader = forwardRef<HTMLDivElement, PopoverHeaderProps>(function PopoverHeader(
  { children, ...rest },
  ref,
) {
  return (
    <StyledPopoverHeader ref={ref} {...rest}>
      {children}
    </StyledPopoverHeader>
  );
});

/** Renders the body section of a Popover. */
export const PopoverBody = forwardRef<HTMLDivElement, PopoverBodyProps>(function PopoverBody(
  { children, ...rest },
  ref,
) {
  return (
    <StyledPopoverBody ref={ref} {...rest}>
      {children}
    </StyledPopoverBody>
  );
});

/** Renders a close button for the Popover. */
export const PopoverCloseButton = forwardRef<HTMLButtonElement, PopoverCloseButtonProps>(function PopoverCloseButton(
  { onClose, onClick, ...rest },
  ref,
) {
  return (
    <CloseButton
      ref={ref}
      type='button'
      aria-label='Close popover'
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
