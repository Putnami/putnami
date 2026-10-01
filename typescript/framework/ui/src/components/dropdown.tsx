'use client';

import {
  Children,
  cloneElement,
  createContext,
  Fragment,
  isValidElement,
  type ReactElement,
  type ReactNode,
  type Ref,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from 'react';
import { createPortal } from 'react-dom';
import { css, keyframes, styled } from '../emotion';
import { isDismissKey, isOutsideClick } from '../hooks/use-dismiss';

// Flatten React children so Fragments and arrays surface their inner elements,
// letting `Dropdown` find its trigger/menu through common wrappers and conditional rendering.
function flattenChildren(children: ReactNode): ReactNode[] {
  return Children.toArray(children).flatMap((child) => {
    if (isValidElement<{ children?: ReactNode }>(child) && child.type === Fragment) {
      return flattenChildren(child.props.children);
    }
    return [child];
  });
}

type DropdownPlacement = 'bottom-start' | 'bottom-end' | 'top-start' | 'top-end';

interface DropdownContextValue {
  isOpen: boolean;
  close: () => void;
}

const DropdownContext = createContext<DropdownContextValue | null>(null);

/** Props for the Dropdown component, a togglable overlay menu triggered by a button. */
export interface DropdownProps {
  placement?: DropdownPlacement;
  offset?: number;
  closeOnSelect?: boolean;
  children: ReactNode;
  className?: string;
}

/** Props for the DropdownTrigger, which wraps the element that toggles the menu. */
export interface DropdownTriggerProps {
  children: ReactElement;
}

/** Props for the DropdownMenu container that holds menu items. */
export interface DropdownMenuProps {
  children?: ReactNode;
  className?: string;
  minWidth?: string;
}

/** Props for an individual dropdown menu item with optional icon and danger styling. */
export interface DropdownItemProps {
  children?: ReactNode;
  icon?: ReactNode;
  isDisabled?: boolean;
  isDanger?: boolean;
  onClick?: () => void;
  className?: string;
}

/** Props for a visual separator between dropdown menu items. */
export interface DropdownDividerProps {
  className?: string;
}

type TriggerChildProps = {
  onClick?: (e: MouseEvent) => void;
  ref?: Ref<HTMLElement>;
  'aria-expanded'?: boolean;
  'aria-haspopup'?: 'menu';
};

const fadeIn = keyframes`
  from {
    opacity: 0;
    transform: translateY(-4px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
`;

const MenuContainer = styled.div<{ $minWidth: string }>`
  position: fixed;
  z-index: 1000;
  background: var(--color-bg);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-lg, 0 10px 15px -3px rgba(0, 0, 0, 0.1));
  padding: var(--space-xs) 0;
  min-width: ${({ $minWidth }) => $minWidth};
  animation: ${fadeIn} 0.15s ease-out;
`;

const StyledDropdownItem = styled.button<{
  $isDisabled?: boolean;
  $isDanger?: boolean;
}>`
  display: flex;
  align-items: center;
  gap: var(--space-sm);
  width: 100%;
  padding: var(--space-sm) var(--space-md);
  background: transparent;
  border: none;
  font-family: var(--font-sans);
  font-size: 0.9rem;
  text-align: left;
  cursor: pointer;
  transition: all var(--transition-fast);
  color: var(--color-text);

  &:hover:not(:disabled) {
    background: var(--color-surface-hover);
  }

  &:focus-visible {
    outline: none;
    background: var(--color-surface-hover);
  }

  ${({ $isDisabled }) =>
    $isDisabled &&
    css`
      opacity: 0.5;
      cursor: not-allowed;
      pointer-events: none;
    `}

  ${({ $isDanger }) =>
    $isDanger &&
    css`
      color: var(--color-error);

      &:hover:not(:disabled) {
        background: var(--color-error);
        color: white;
      }
    `}
`;

const IconWrapper = styled.span`
  display: flex;
  align-items: center;
  justify-content: center;
  width: 16px;
  height: 16px;
  flex-shrink: 0;

  svg {
    width: 100%;
    height: 100%;
  }
`;

const StyledDropdownDivider = styled.div`
  height: 1px;
  background: var(--color-border);
  margin: var(--space-xs) 0;
`;

/** Renders a dropdown menu with a trigger element, positioned relative to the trigger. */
export function Dropdown({ placement = 'bottom-start', offset = 4, closeOnSelect = true, children }: DropdownProps) {
  const [isOpen, setIsOpen] = useState(false);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  const triggerRef = useRef<HTMLElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  const close = useCallback(() => setIsOpen(false), []);
  const toggle = useCallback(() => setIsOpen((prev) => !prev), []);

  const calculatePosition = useCallback(() => {
    if (!triggerRef.current || !menuRef.current) return;

    const triggerRect = triggerRef.current.getBoundingClientRect();
    const menuRect = menuRef.current.getBoundingClientRect();

    let top = 0;
    let left = 0;

    if (placement.startsWith('bottom')) {
      top = triggerRect.bottom + offset;
    } else {
      top = triggerRect.top - menuRect.height - offset;
    }

    if (placement.endsWith('start')) {
      left = triggerRect.left;
    } else {
      left = triggerRect.right - menuRect.width;
    }

    // Viewport boundary checks
    const padding = 8;
    if (left < padding) left = padding;
    if (left + menuRect.width > window.innerWidth - padding) {
      left = window.innerWidth - menuRect.width - padding;
    }
    if (top < padding) {
      top = triggerRect.bottom + offset;
    }
    if (top + menuRect.height > window.innerHeight - padding) {
      top = triggerRect.top - menuRect.height - offset;
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
    if (isOpen) {
      const handleClickOutside = (e: MouseEvent) => {
        if (isOutsideClick(e.target, menuRef.current, triggerRef.current)) {
          close();
        }
      };

      const handleEsc = (e: KeyboardEvent) => {
        if (isDismissKey(e.key)) {
          close();
        }
      };

      document.addEventListener('mousedown', handleClickOutside);
      document.addEventListener('keydown', handleEsc);

      return () => {
        document.removeEventListener('mousedown', handleClickOutside);
        document.removeEventListener('keydown', handleEsc);
      };
    }
    return undefined;
  }, [isOpen, close]);

  // Find trigger and menu from children (handles Fragments, arrays, and conditional rendering).
  let trigger: ReactElement<DropdownTriggerProps> | null = null;
  let menu: ReactElement<DropdownMenuProps> | null = null;

  for (const child of flattenChildren(children)) {
    if (!isValidElement(child)) continue;
    if (child.type === DropdownTrigger) {
      trigger = child as ReactElement<DropdownTriggerProps>;
    } else if (child.type === DropdownMenu) {
      menu = child as ReactElement<DropdownMenuProps>;
    }
  }

  return (
    <DropdownContext.Provider value={{ isOpen, close: closeOnSelect ? close : () => {} }}>
      {!!trigger &&
        (() => {
          const triggerChild = trigger.props.children as ReactElement<TriggerChildProps>;
          const triggerOnClick = triggerChild.props.onClick;
          return cloneElement(triggerChild, {
            ref: triggerRef as Ref<HTMLElement>,
            onClick: (e: MouseEvent) => {
              toggle();
              triggerOnClick?.(e);
            },
            'aria-expanded': isOpen,
            'aria-haspopup': 'menu',
          });
        })()}
      {!!isOpen &&
        !!menu &&
        typeof document !== 'undefined' &&
        createPortal(
          <MenuContainer
            ref={menuRef}
            $minWidth={menu.props.minWidth || '180px'}
            className={menu.props.className}
            role='menu'
            style={{ top: position.top, left: position.left }}
          >
            {menu.props.children}
          </MenuContainer>,
          document.body,
        )}
    </DropdownContext.Provider>
  );
}

/** Wraps the element that toggles the dropdown menu open/closed. */
export function DropdownTrigger({ children }: DropdownTriggerProps) {
  return children;
}

/** Container for dropdown menu items, rendered inside a portal. */
export function DropdownMenu({ children }: DropdownMenuProps) {
  return <>{children}</>;
}

/** Renders an interactive menu item inside a dropdown. */
export function DropdownItem({
  children,
  icon,
  isDisabled = false,
  isDanger = false,
  onClick,
  className,
}: DropdownItemProps) {
  const context = useContext(DropdownContext);

  const handleClick = () => {
    if (!isDisabled) {
      onClick?.();
      context?.close();
    }
  };

  return (
    <StyledDropdownItem
      $isDisabled={isDisabled}
      $isDanger={isDanger}
      onClick={handleClick}
      className={className}
      role='menuitem'
      disabled={isDisabled}
      type='button'
    >
      {!!icon && <IconWrapper>{icon}</IconWrapper>}
      {children}
    </StyledDropdownItem>
  );
}

/** Renders a horizontal separator line between dropdown menu items. */
export function DropdownDivider({ className }: DropdownDividerProps) {
  return <StyledDropdownDivider className={className} role='separator' />;
}
