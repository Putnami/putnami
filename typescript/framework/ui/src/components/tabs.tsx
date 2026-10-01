'use client';

import {
  Children,
  cloneElement,
  type ComponentProps,
  createContext,
  Fragment,
  isValidElement,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
  useContext,
  useState,
} from 'react';
import { css, styled } from '../emotion';
import { Box, type BoxProps } from '../layout';
import type { SemanticColor } from '../theme/theme';

// Flatten React children so that Fragments, arrays, and `.map(...)` outputs are
// walked as a single sequence. Children.toArray normalizes arrays/iterables but
// leaves Fragments as opaque elements, so we recurse into them ourselves.
function flattenChildren(children: ReactNode): ReactNode[] {
  return Children.toArray(children).flatMap((child) => {
    if (isValidElement<{ children?: ReactNode }>(child) && child.type === Fragment) {
      return flattenChildren(child.props.children);
    }
    return [child];
  });
}

type TabsVariant = 'line' | 'enclosed' | 'soft-rounded';
type TabsSize = 'sm' | 'md' | 'lg';
type TabsColorScheme = 'primary' | 'secondary' | 'gray';

interface TabsContextValue {
  activeIndex: number;
  setActiveIndex: (index: number) => void;
  variant: TabsVariant;
  size: TabsSize;
  colorScheme: TabsColorScheme;
}

const TabsContext = createContext<TabsContextValue | null>(null);

const useTabsContext = () => {
  const context = useContext(TabsContext);
  if (!context) {
    throw new Error('Tabs components must be used within a Tabs provider');
  }
  return context;
};

// Per-TabList context carrying the disabled state of every tab, keyed by index.
// `Tab` reads it so its keyboard handler can skip disabled tabs when moving focus.
// Provided by `TabList` (which already walks its children to assign indices) rather
// than by `Tabs`, so the surrounding `Tabs` context stays unchanged.
const TabListContext = createContext<boolean[]>([]);

/**
 * Computes the index of the tab a keyboard navigation key should move focus to,
 * skipping disabled tabs and wrapping around the ends. Returns `null` for keys that are
 * not navigation keys, and when there is no enabled tab to move to (e.g. every other
 * tab is disabled).
 *
 * `disabled[i]` is whether the tab at index `i` is disabled. Pure (no DOM) so the
 * roving-focus rule is unit-testable without a browser environment.
 */
export function nextEnabledTabIndex(disabled: readonly boolean[], current: number, key: string): number | null {
  const count = disabled.length;
  if (count === 0) return null;

  const isEnabled = (i: number) => !disabled[i];

  if (key === 'Home') {
    for (let i = 0; i < count; i++) {
      if (isEnabled(i)) return i;
    }
    return null;
  }
  if (key === 'End') {
    for (let i = count - 1; i >= 0; i--) {
      if (isEnabled(i)) return i;
    }
    return null;
  }

  if (key !== 'ArrowRight' && key !== 'ArrowLeft') return null;
  const step = key === 'ArrowRight' ? 1 : -1;
  // Walk at most `count` steps so a fully-disabled list terminates instead of looping.
  for (let offset = 1; offset <= count; offset++) {
    const candidate = (current + step * offset + count * offset) % count;
    if (isEnabled(candidate)) return candidate;
  }
  return null;
}

/** Props for the Tabs component, a tabbed content switcher with line, enclosed, and soft-rounded variants. */
export interface TabsProps extends BoxProps {
  defaultIndex?: number;
  index?: number;
  onChange?: (index: number) => void;
  variant?: TabsVariant;
  size?: TabsSize;
  colorScheme?: TabsColorScheme;
  children?: ReactNode;
}

/** Props for the TabList container that holds Tab elements. */
export interface TabListProps extends BoxProps {
  children?: ReactNode;
}

/** Props for an individual Tab button. */
export interface TabProps extends BoxProps {
  children?: ReactNode;
  isDisabled?: boolean;
  index?: number;
}

/** Props for the TabPanels container that holds TabPanel elements. */
export interface TabPanelsProps extends BoxProps {
  children?: ReactNode;
}

/** Props for an individual TabPanel content area. */
export interface TabPanelProps extends BoxProps {
  children?: ReactNode;
  index?: number;
}

const sizeStyles: Record<TabsSize, { padding: string; fontSize: string }> = {
  sm: { padding: 'var(--space-xs) var(--space-sm)', fontSize: '0.8rem' },
  md: { padding: 'var(--space-sm) var(--space-md)', fontSize: '0.9rem' },
  lg: { padding: 'var(--space-md) var(--space-lg)', fontSize: '1rem' },
};

const StyledTabList = styled(Box)<{ $variant: TabsVariant }>`
  display: flex;
  position: relative;

  ${({ $variant }) =>
    $variant === 'line' &&
    css`
      border-bottom: 2px solid var(--color-border);
    `}

  ${({ $variant }) =>
    $variant === 'enclosed' &&
    css`
      border-bottom: 1px solid var(--color-border);
    `}

  ${({ $variant }) =>
    $variant === 'soft-rounded' &&
    css`
      background: var(--color-surface);
      border-radius: var(--radius-lg);
      padding: var(--space-xs);
      gap: var(--space-xs);
    `}
`;

const StyledTab = styled.button<{
  $variant: TabsVariant;
  $size: TabsSize;
  $colorScheme: TabsColorScheme;
  $isActive: boolean;
  $isDisabled: boolean;
}>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-family: var(--font-sans);
  font-weight: 500;
  border: none;
  background: transparent;
  cursor: pointer;
  transition: all var(--transition-fast);
  white-space: nowrap;
  color: var(--color-text-muted);

  ${({ $size }) => css`
    padding: ${sizeStyles[$size].padding};
    font-size: ${sizeStyles[$size].fontSize};
  `}

  &:focus-visible {
    outline: 2px solid var(--color-primary);
    outline-offset: 2px;
  }

  ${({ $isDisabled }) =>
    $isDisabled &&
    css`
      opacity: 0.5;
      cursor: not-allowed;
    `}

  /* Variant: line */
  ${({ $variant, $isActive, theme, $colorScheme }) =>
    $variant === 'line' &&
    css`
      margin-bottom: -2px;
      border-bottom: 2px solid transparent;

      ${
        $isActive &&
        css`
        color: ${$colorScheme === 'gray' ? 'var(--color-text)' : (theme.colors[$colorScheme] as SemanticColor).main};
        border-bottom-color: ${$colorScheme === 'gray' ? 'var(--color-text)' : (theme.colors[$colorScheme] as SemanticColor).main};
      `
      }

      &:hover:not(:disabled) {
        color: ${$colorScheme === 'gray' ? 'var(--color-text)' : (theme.colors[$colorScheme] as SemanticColor).main};
      }
    `}

  /* Variant: enclosed */
  ${({ $variant, $isActive }) =>
    $variant === 'enclosed' &&
    css`
      border: 1px solid transparent;
      border-bottom: none;
      margin-bottom: -1px;
      border-radius: var(--radius-md) var(--radius-md) 0 0;

      ${
        $isActive &&
        css`
        color: var(--color-text);
        background: var(--color-bg);
        border-color: var(--color-border);
      `
      }

      &:hover:not(:disabled) {
        color: var(--color-text);
      }
    `}

  /* Variant: soft-rounded */
  ${({ $variant, $isActive, theme, $colorScheme }) =>
    $variant === 'soft-rounded' &&
    css`
      border-radius: var(--radius-md);

      ${
        $isActive &&
        css`
        color: ${$colorScheme === 'gray' ? 'var(--color-text)' : (theme.colors[$colorScheme] as SemanticColor).main};
        background: var(--color-bg);
        box-shadow: var(--shadow-sm, 0 1px 2px rgba(0, 0, 0, 0.05));
      `
      }

      &:hover:not(:disabled) {
        color: ${$isActive ? ($colorScheme === 'gray' ? 'var(--color-text)' : (theme.colors[$colorScheme] as SemanticColor).main) : 'var(--color-text)'};
      }
    `}
`;

const StyledTabPanels = styled(Box)`
  margin-top: var(--space-md);
`;

const StyledTabPanel = styled(Box)<{ $isActive: boolean }>`
  display: ${({ $isActive }) => ($isActive ? 'block' : 'none')};
`;

/** Renders a tabbed interface with controlled or uncontrolled active index. */
export function Tabs({
  defaultIndex = 0,
  index,
  onChange,
  variant = 'line',
  size = 'md',
  colorScheme = 'primary',
  children,
  ...props
}: TabsProps & ComponentProps<typeof Box>) {
  const [internalIndex, setInternalIndex] = useState(defaultIndex);
  const activeIndex = index !== undefined ? index : internalIndex;

  const setActiveIndex = (newIndex: number) => {
    if (index === undefined) {
      setInternalIndex(newIndex);
    }
    onChange?.(newIndex);
  };

  return (
    <TabsContext.Provider value={{ activeIndex, setActiveIndex, variant, size, colorScheme }}>
      <Box {...props}>{children}</Box>
    </TabsContext.Provider>
  );
}

/**
 * Renders the row of tab buttons, automatically assigning indices to Tab children.
 *
 * Tabs must be either direct children of `TabList`, included via `Array.prototype.map`,
 * or wrapped in Fragments — any other wrapper component will not receive an index.
 */
export function TabList({ children, ...props }: TabListProps & ComponentProps<typeof Box>) {
  const { variant } = useTabsContext();

  let tabIndex = 0;
  const disabledByIndex: boolean[] = [];
  const childrenWithIndex = flattenChildren(children).map((child) => {
    if (isValidElement<TabProps>(child) && child.type === Tab) {
      const index = tabIndex++;
      disabledByIndex[index] = child.props.isDisabled ?? false;
      return cloneElement(child, { index });
    }
    return child;
  });

  return (
    <TabListContext.Provider value={disabledByIndex}>
      <StyledTabList $variant={variant} role='tablist' {...props}>
        {childrenWithIndex}
      </StyledTabList>
    </TabListContext.Provider>
  );
}

/** Renders a single tab button that activates its corresponding panel. */
export function Tab({ children, isDisabled = false, index = 0 }: TabProps) {
  const { activeIndex, setActiveIndex, variant, size, colorScheme } = useTabsContext();
  const disabledByIndex = useContext(TabListContext);
  const isActive = activeIndex === index;

  // Implements the WAI-ARIA Tabs roving-focus contract: with a roving tabIndex the
  // inactive tabs are tabIndex=-1, so the arrow keys (and Home/End) are the only way
  // to reach them. Move selection to the next enabled tab and focus its button.
  const handleKeyDown = (event: ReactKeyboardEvent<HTMLButtonElement>) => {
    const target = nextEnabledTabIndex(disabledByIndex, index, event.key);
    if (target === null || target === index) return;
    event.preventDefault();
    setActiveIndex(target);
    // Focus the newly active tab button. The tabs are siblings inside the tablist,
    // so query them from the current button's parent (runs only on a real keypress).
    const tablist = event.currentTarget.closest('[role="tablist"]');
    const buttons = tablist?.querySelectorAll<HTMLButtonElement>('[role="tab"]');
    buttons?.[target]?.focus();
  };

  return (
    <StyledTab
      $variant={variant}
      $size={size}
      $colorScheme={colorScheme}
      $isActive={isActive}
      $isDisabled={isDisabled}
      disabled={isDisabled}
      onClick={() => !isDisabled && setActiveIndex(index)}
      onKeyDown={handleKeyDown}
      role='tab'
      aria-selected={isActive}
      tabIndex={isActive ? 0 : -1}
      type='button'
    >
      {children}
    </StyledTab>
  );
}

/**
 * Renders the container for tab panels, automatically assigning indices to TabPanel children.
 *
 * Panels must be either direct children of `TabPanels`, included via `Array.prototype.map`,
 * or wrapped in Fragments — any other wrapper component will not receive an index.
 */
export function TabPanels({ children, ...props }: TabPanelsProps & ComponentProps<typeof Box>) {
  let panelIndex = 0;
  const childrenWithIndex = flattenChildren(children).map((child) => {
    if (isValidElement<TabPanelProps>(child) && child.type === TabPanel) {
      return cloneElement(child, { index: panelIndex++ });
    }
    return child;
  });

  return <StyledTabPanels {...props}>{childrenWithIndex}</StyledTabPanels>;
}

/** Renders a tab panel that is visible only when its index matches the active tab. */
export function TabPanel({ children, index = 0, ...props }: TabPanelProps & ComponentProps<typeof Box>) {
  const { activeIndex } = useTabsContext();
  const isActive = activeIndex === index;

  return (
    <StyledTabPanel $isActive={isActive} role='tabpanel' hidden={!isActive} {...props}>
      {children}
    </StyledTabPanel>
  );
}
