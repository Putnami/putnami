'use client';

import {
  cloneElement,
  type ReactElement,
  type FocusEvent as ReactFocusEvent,
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
import { createTooltipTimer, type TooltipTimer } from './tooltip-timing';

type TooltipPlacement = 'top' | 'bottom' | 'left' | 'right';

type TriggerChildProps = {
  onMouseEnter?: (event: ReactMouseEvent<HTMLElement>) => void;
  onMouseLeave?: (event: ReactMouseEvent<HTMLElement>) => void;
  onFocus?: (event: ReactFocusEvent<HTMLElement>) => void;
  onBlur?: (event: ReactFocusEvent<HTMLElement>) => void;
  ref?: Ref<HTMLElement>;
};

/** Props for the Tooltip component, a hover-triggered floating text label. */
export interface TooltipProps {
  label: ReactNode;
  placement?: TooltipPlacement;
  delay?: number;
  offset?: number;
  isDisabled?: boolean;
  children: ReactElement<TriggerChildProps>;
  className?: string;
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

const TooltipContainer = styled.div<{ $placement: TooltipPlacement }>`
  position: fixed;
  z-index: 1500;
  padding: var(--space-xs) var(--space-sm);
  background: var(--color-text);
  color: var(--color-bg);
  font-family: var(--font-sans);
  font-size: 0.8rem;
  font-weight: 500;
  border-radius: var(--radius-md);
  max-width: 250px;
  word-wrap: break-word;
  pointer-events: none;
  animation: ${fadeIn} 0.15s ease-out;
  box-shadow: var(--shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));

  /* Arrow */
  &::after {
    content: '';
    position: absolute;
    border: 5px solid transparent;

    ${({ $placement }) => {
      switch ($placement) {
        case 'top':
          return css`
            bottom: -10px;
            left: 50%;
            transform: translateX(-50%);
            border-top-color: var(--color-text);
          `;
        case 'bottom':
          return css`
            top: -10px;
            left: 50%;
            transform: translateX(-50%);
            border-bottom-color: var(--color-text);
          `;
        case 'left':
          return css`
            right: -10px;
            top: 50%;
            transform: translateY(-50%);
            border-left-color: var(--color-text);
          `;
        case 'right':
          return css`
            left: -10px;
            top: 50%;
            transform: translateY(-50%);
            border-right-color: var(--color-text);
          `;
      }
    }}
  }
`;

/** Renders a floating tooltip label that appears on hover or focus of the trigger element. */
export function Tooltip({
  label,
  placement = 'top',
  delay = 200,
  offset = 8,
  isDisabled = false,
  children,
  className,
}: TooltipProps) {
  const [isVisible, setIsVisible] = useState(false);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  const triggerRef = useRef<HTMLElement>(null);
  const tooltipRef = useRef<HTMLDivElement>(null);

  // Show/hide scheduling lives in a pure, unit-tested controller (tooltip-timing.ts).
  // `setIsVisible` is stable across renders, so the controller is created once.
  const timerRef = useRef<TooltipTimer | null>(null);
  if (timerRef.current === null) {
    timerRef.current = createTooltipTimer({
      onShow: () => setIsVisible(true),
      onHide: () => setIsVisible(false),
    });
  }

  const calculatePosition = useCallback(() => {
    if (!triggerRef.current || !tooltipRef.current) return;

    const triggerRect = triggerRef.current.getBoundingClientRect();
    const tooltipRect = tooltipRef.current.getBoundingClientRect();

    let top = 0;
    let left = 0;

    switch (placement) {
      case 'top':
        top = triggerRect.top - tooltipRect.height - offset;
        left = triggerRect.left + triggerRect.width / 2 - tooltipRect.width / 2;
        break;
      case 'bottom':
        top = triggerRect.bottom + offset;
        left = triggerRect.left + triggerRect.width / 2 - tooltipRect.width / 2;
        break;
      case 'left':
        top = triggerRect.top + triggerRect.height / 2 - tooltipRect.height / 2;
        left = triggerRect.left - tooltipRect.width - offset;
        break;
      case 'right':
        top = triggerRect.top + triggerRect.height / 2 - tooltipRect.height / 2;
        left = triggerRect.right + offset;
        break;
    }

    // Viewport boundary checks
    const padding = 8;
    if (left < padding) left = padding;
    if (left + tooltipRect.width > window.innerWidth - padding) {
      left = window.innerWidth - tooltipRect.width - padding;
    }
    if (top < padding) top = padding;
    if (top + tooltipRect.height > window.innerHeight - padding) {
      top = window.innerHeight - tooltipRect.height - padding;
    }

    setPosition({ top, left });
  }, [placement, offset]);

  useEffect(() => {
    if (isVisible) {
      calculatePosition();
      window.addEventListener('scroll', calculatePosition, true);
      window.addEventListener('resize', calculatePosition);

      return () => {
        window.removeEventListener('scroll', calculatePosition, true);
        window.removeEventListener('resize', calculatePosition);
      };
    }
    return undefined;
  }, [isVisible, calculatePosition]);

  const showTooltip = () => timerRef.current?.show(delay, isDisabled);
  const hideTooltip = () => timerRef.current?.hide();

  useEffect(() => () => timerRef.current?.cancel(), []);

  const trigger = cloneElement(children, {
    ref: triggerRef as Ref<HTMLElement>,
    onMouseEnter: (event: ReactMouseEvent<HTMLElement>) => {
      showTooltip();
      children.props.onMouseEnter?.(event);
    },
    onMouseLeave: (event: ReactMouseEvent<HTMLElement>) => {
      hideTooltip();
      children.props.onMouseLeave?.(event);
    },
    onFocus: (event: ReactFocusEvent<HTMLElement>) => {
      showTooltip();
      children.props.onFocus?.(event);
    },
    onBlur: (event: ReactFocusEvent<HTMLElement>) => {
      hideTooltip();
      children.props.onBlur?.(event);
    },
  });

  return (
    <>
      {trigger}
      {!!isVisible &&
        typeof document !== 'undefined' &&
        createPortal(
          <TooltipContainer
            ref={tooltipRef}
            $placement={placement}
            className={className}
            role='tooltip'
            style={{ top: position.top, left: position.left }}
          >
            {label}
          </TooltipContainer>,
          document.body,
        )}
    </>
  );
}
