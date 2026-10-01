'use client';

import { useCallback, useEffect, useState } from 'react';
import { styled } from '../emotion';
import { Box, Flex } from '../layout';

/** Represents a documentation topic with rendered HTML content and table of contents entries. */
export interface TopicDoc {
  content: string;
  toc: TocItem[];
}

/** Represents a single heading entry in a table of contents. */
export interface TocItem {
  level: number;
  id: string;
  text: string;
}

interface TocProps {
  items: TocItem[];
  className?: string;
}

const StyledTocNav = styled(Box)`
  position: sticky;
  top: 88px; /* Sticks below navbar */
  align-self: flex-start;
  margin-top: 96px; /* Align with bottom of H1 heading */
  margin-left: var(--space-xl);
  padding-left: var(--space-lg);
  border-left: 2px solid var(--color-border);
`;

interface ActiveProps {
  $isActive?: boolean;
  $level?: number;
}

const StyledLink = styled.a<ActiveProps>`
  color: ${(props: ActiveProps) => (props.$isActive ? 'var(--color-primary)' : 'var(--color-text-muted)')};
  text-decoration: none;
  transition: all 200ms ease;
  display: block;
  padding: var(--space-xs) 0;
  position: relative;

  /* Active indicator bar - position compensates for indentation to stay aligned */
  &::before {
    content: '';
    position: absolute;
    left: calc(-1 * var(--space-lg) - 2px - ${(props: ActiveProps) => (props.$level && props.$level > 1 ? `(${props.$level - 1} * var(--space-md))` : '0px')});
    top: 50%;
    transform: translateY(-50%);
    width: 2px;
    height: ${(props: ActiveProps) => (props.$isActive ? '100%' : '0%')};
    background: var(--color-primary);
    transition: height 200ms ease;
  }

  &:hover {
    color: ${(props: ActiveProps) => (props.$isActive ? 'var(--color-primary)' : 'var(--color-text)')};
  }
`;

const StyledListItem = styled(Box)<ActiveProps>`
  font-size: 0.85rem;
  transition: transform 150ms ease;
  transform: ${(props: ActiveProps) => (props.$isActive ? 'translateX(2px)' : 'translateX(0)')};
`;

const getIntersectingId = (entries: IntersectionObserverEntry[]): string | undefined => {
  const visibleEntries = entries.filter((entry) => entry.isIntersecting);
  if (visibleEntries.length === 0) {
    return undefined;
  }

  visibleEntries.sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
  return visibleEntries[0]?.target.id;
};

const observeItems = (items: TocItem[], observer: IntersectionObserver) => {
  for (const item of items) {
    const element = document.getElementById(item.id);
    if (element) {
      observer.observe(element);
    }
  }
};

const getInitialActiveId = (items: TocItem[]): string | undefined => {
  const viewportHeight = window.innerHeight;

  for (const item of items) {
    const rect = document.getElementById(item.id)?.getBoundingClientRect();
    if (rect && rect.top >= 0 && rect.top < viewportHeight * 0.5) {
      return item.id;
    }
  }

  return items[0]?.id;
};

/** Renders a sticky table of contents sidebar that highlights the currently visible section. */
export function MarkdownToc({ items, className = '' }: TocProps) {
  const [activeId, setActiveId] = useState<string>('');

  const handleIntersection = useCallback((entries: IntersectionObserverEntry[]) => {
    const nextActiveId = getIntersectingId(entries);
    if (nextActiveId) {
      setActiveId(nextActiveId);
    }
  }, []);

  useEffect(() => {
    if (items.length === 0) return;

    const observer = new IntersectionObserver(handleIntersection, {
      rootMargin: '-80px 0px -60% 0px', // Account for navbar and trigger when section is in upper portion
      threshold: 0,
    });

    observeItems(items, observer);

    return () => observer.disconnect();
  }, [items, handleIntersection]);

  useEffect(() => {
    if (items.length === 0 || activeId) {
      return;
    }

    const nextActiveId = getInitialActiveId(items);
    if (nextActiveId) {
      setActiveId(nextActiveId);
    }
  }, [items, activeId]);

  if (items.length === 0) {
    return null;
  }

  return (
    <StyledTocNav as='nav' className={className} display={['none', 'none', 'none', 'block']}>
      <Box
        as='h3'
        mb='md'
        color='text.secondary'
        style={{
          fontSize: '0.75rem',
          fontWeight: 600,
          textTransform: 'uppercase',
          letterSpacing: '0.1em',
          color: 'var(--color-text-dim)',
          marginBottom: 'var(--space-md)',
        }}
      >
        On this page
      </Box>
      <Flex as='ul' direction='column' gap='xs' style={{ listStyle: 'none', padding: 0, margin: 0 }}>
        {items.map((item) => {
          const isActive = activeId === item.id;
          return (
            <StyledListItem
              as='li'
              key={`${item.id}-key`}
              $isActive={isActive}
              style={{
                paddingLeft: item.level === 1 ? '0' : `calc(${item.level - 1} * var(--space-md))`,
                fontWeight: item.level === 1 ? 500 : 'normal',
              }}
            >
              <StyledLink href={`#${item.id}`} $isActive={isActive} $level={item.level}>
                {item.text}
              </StyledLink>
            </StyledListItem>
          );
        })}
      </Flex>
    </StyledTocNav>
  );
}
