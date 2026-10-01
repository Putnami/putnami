'use client';

import { useMemo } from 'react';
import { css, styled } from '../emotion';

type PaginationSize = 'sm' | 'md' | 'lg';

/** Props for the Pagination component, page navigation with previous/next and numbered page buttons. */
export interface PaginationProps {
  page: number;
  totalPages: number;
  onChange?: (page: number) => void;
  size?: PaginationSize;
  siblingCount?: number;
  showFirstLast?: boolean;
  className?: string;
}

const sizeStyles: Record<PaginationSize, { size: string; fontSize: string }> = {
  sm: { size: '28px', fontSize: '0.8rem' },
  md: { size: '36px', fontSize: '0.9rem' },
  lg: { size: '44px', fontSize: '1rem' },
};

const PaginationContainer = styled.nav`
  display: flex;
  align-items: center;
  gap: var(--space-xs);
  font-family: var(--font-sans);
`;

const PageButton = styled.button<{
  $size: PaginationSize;
  $isActive?: boolean;
  $isDisabled?: boolean;
}>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: 1px solid var(--color-border);
  background: var(--color-bg);
  color: var(--color-text);
  border-radius: var(--radius-md);
  cursor: pointer;
  transition: all var(--transition-fast);
  font-weight: 500;

  ${({ $size }) => css`
    min-width: ${sizeStyles[$size].size};
    height: ${sizeStyles[$size].size};
    padding: 0 var(--space-sm);
    font-size: ${sizeStyles[$size].fontSize};
  `}

  &:hover:not(:disabled) {
    background: var(--color-surface-hover);
    border-color: var(--color-text-muted);
  }

  &:focus-visible {
    outline: 2px solid var(--color-primary);
    outline-offset: 2px;
  }

  ${({ $isActive }) =>
    $isActive &&
    css`
      background: var(--color-primary);
      color: white;
      border-color: var(--color-primary);

      &:hover:not(:disabled) {
        background: var(--color-primary-dark);
        border-color: var(--color-primary-dark);
      }
    `}

  ${({ $isDisabled }) =>
    $isDisabled &&
    css`
      opacity: 0.5;
      cursor: not-allowed;
    `}
`;

const Ellipsis = styled.span<{ $size: PaginationSize }>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: var(--color-text-muted);

  ${({ $size }) => css`
    min-width: ${sizeStyles[$size].size};
    height: ${sizeStyles[$size].size};
    font-size: ${sizeStyles[$size].fontSize};
  `}
`;

const prevIcon = (
  <svg width='16' height='16' viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Previous</title>
    <polyline points='15 18 9 12 15 6' />
  </svg>
);

const nextIcon = (
  <svg width='16' height='16' viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Next</title>
    <polyline points='9 18 15 12 9 6' />
  </svg>
);

const firstIcon = (
  <svg width='16' height='16' viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>First</title>
    <polyline points='11 17 6 12 11 7' />
    <polyline points='18 17 13 12 18 7' />
  </svg>
);

const lastIcon = (
  <svg width='16' height='16' viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Last</title>
    <polyline points='13 17 18 12 13 7' />
    <polyline points='6 17 11 12 6 7' />
  </svg>
);

function usePagination(page: number, totalPages: number, siblingCount: number) {
  return useMemo(() => {
    const range = (start: number, end: number) => Array.from({ length: end - start + 1 }, (_, i) => start + i);

    const totalNumbers = siblingCount * 2 + 3; // siblings + current + first + last
    const totalBlocks = totalNumbers + 2; // + 2 for ellipses

    if (totalPages <= totalBlocks) {
      return range(1, totalPages);
    }

    const leftSiblingIndex = Math.max(page - siblingCount, 1);
    const rightSiblingIndex = Math.min(page + siblingCount, totalPages);

    const showLeftEllipsis = leftSiblingIndex > 2;
    const showRightEllipsis = rightSiblingIndex < totalPages - 1;

    if (!showLeftEllipsis && showRightEllipsis) {
      const leftRange = range(1, totalNumbers);
      return [...leftRange, 'ellipsis-right', totalPages];
    }

    if (showLeftEllipsis && !showRightEllipsis) {
      const rightRange = range(totalPages - totalNumbers + 1, totalPages);
      return [1, 'ellipsis-left', ...rightRange];
    }

    const middleRange = range(leftSiblingIndex, rightSiblingIndex);
    return [1, 'ellipsis-left', ...middleRange, 'ellipsis-right', totalPages];
  }, [page, totalPages, siblingCount]);
}

/** Renders page navigation controls with numbered buttons, ellipsis gaps, and prev/next arrows. */
export function Pagination({
  page,
  totalPages,
  onChange,
  size = 'md',
  siblingCount = 1,
  showFirstLast = false,
  className,
}: PaginationProps) {
  const pages = usePagination(page, totalPages, siblingCount);

  const handlePageChange = (newPage: number) => {
    if (newPage >= 1 && newPage <= totalPages && newPage !== page) {
      onChange?.(newPage);
    }
  };

  if (totalPages <= 1) {
    return null;
  }

  return (
    <PaginationContainer className={className} aria-label='Pagination'>
      {!!showFirstLast && (
        <PageButton
          $size={size}
          $isDisabled={page === 1}
          disabled={page === 1}
          onClick={() => handlePageChange(1)}
          aria-label='First page'
          type='button'
        >
          {firstIcon}
        </PageButton>
      )}

      <PageButton
        $size={size}
        $isDisabled={page === 1}
        disabled={page === 1}
        onClick={() => handlePageChange(page - 1)}
        aria-label='Previous page'
        type='button'
      >
        {prevIcon}
      </PageButton>

      {pages.map((p, _index) => {
        if (typeof p === 'string') {
          return (
            <Ellipsis key={p} $size={size}>
              …
            </Ellipsis>
          );
        }

        return (
          <PageButton
            key={p}
            $size={size}
            $isActive={p === page}
            onClick={() => handlePageChange(p)}
            aria-label={`Page ${p}`}
            aria-current={p === page ? 'page' : undefined}
            type='button'
          >
            {p}
          </PageButton>
        );
      })}

      <PageButton
        $size={size}
        $isDisabled={page === totalPages}
        disabled={page === totalPages}
        onClick={() => handlePageChange(page + 1)}
        aria-label='Next page'
        type='button'
      >
        {nextIcon}
      </PageButton>

      {!!showFirstLast && (
        <PageButton
          $size={size}
          $isDisabled={page === totalPages}
          disabled={page === totalPages}
          onClick={() => handlePageChange(totalPages)}
          aria-label='Last page'
          type='button'
        >
          {lastIcon}
        </PageButton>
      )}
    </PaginationContainer>
  );
}
