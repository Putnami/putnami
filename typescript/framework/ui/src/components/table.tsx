import type { ComponentProps, ReactNode, TdHTMLAttributes, ThHTMLAttributes } from 'react';
import { css, styled } from '../emotion';
import { Box, type BoxProps } from '../layout';

type TableVariant = 'simple' | 'striped';
type TableSize = 'sm' | 'md' | 'lg';

/** Props for the Table component, a data table with simple and striped variants. */
export interface TableProps extends BoxProps {
  variant?: TableVariant;
  size?: TableSize;
  children?: ReactNode;
}

/** Props for the TableHead section. */
export interface TableHeadProps {
  children?: ReactNode;
  className?: string;
}

/** Props for the TableBody section. */
export interface TableBodyProps {
  children?: ReactNode;
  className?: string;
}

/** Props for the TableFoot section. */
export interface TableFootProps {
  children?: ReactNode;
  className?: string;
}

/** Props for a table row with optional selected state. */
export interface TableRowProps {
  children?: ReactNode;
  className?: string;
  isSelected?: boolean;
}

/** Props for a table data cell with optional numeric alignment. */
export interface TableCellProps extends TdHTMLAttributes<HTMLTableCellElement> {
  children?: ReactNode;
  isNumeric?: boolean;
}

/** Props for a table header cell with optional sorting controls. */
export interface TableHeaderCellProps extends ThHTMLAttributes<HTMLTableHeaderCellElement> {
  children?: ReactNode;
  isNumeric?: boolean;
  isSortable?: boolean;
  sortDirection?: 'asc' | 'desc' | null;
  onSort?: () => void;
}

const sizeStyles: Record<TableSize, { padding: string; fontSize: string }> = {
  sm: { padding: 'var(--space-xs) var(--space-sm)', fontSize: '0.8rem' },
  md: { padding: 'var(--space-sm) var(--space-md)', fontSize: '0.9rem' },
  lg: { padding: 'var(--space-md) var(--space-lg)', fontSize: '1rem' },
};

const TableContainer = styled(Box)`
  width: 100%;
  overflow-x: auto;
`;

const StyledTable = styled.table<{ $variant: TableVariant; $size: TableSize }>`
  width: 100%;
  border-collapse: collapse;
  font-family: var(--font-sans);

  ${({ $size }) => css`
    font-size: ${sizeStyles[$size].fontSize};
  `}

  th, td {
    ${({ $size }) => css`
      padding: ${sizeStyles[$size].padding};
    `}
  }

  /* Variant: striped */
  ${({ $variant }) =>
    $variant === 'striped' &&
    css`
      tbody tr:nth-of-type(odd) {
        background: var(--color-surface);
      }
    `}
`;

const StyledTableHead = styled.thead`
  border-bottom: 2px solid var(--color-border);
`;

const StyledTableBody = styled.tbody`
  tr {
    border-bottom: 1px solid var(--color-border);

    &:last-child {
      border-bottom: none;
    }
  }
`;

const StyledTableFoot = styled.tfoot`
  border-top: 2px solid var(--color-border);
  font-weight: 500;
`;

const StyledTableRow = styled.tr<{ $isSelected?: boolean }>`
  transition: background var(--transition-fast);

  &:hover {
    background: var(--color-surface-hover);
  }

  ${({ $isSelected }) =>
    $isSelected &&
    css`
      background: var(--color-primary-light, var(--color-surface-hover)) !important;
    `}
`;

const StyledTableCell = styled.td<{ $isNumeric?: boolean }>`
  text-align: left;
  color: var(--color-text);
  vertical-align: middle;

  ${({ $isNumeric }) =>
    $isNumeric &&
    css`
      text-align: right;
      font-variant-numeric: tabular-nums;
    `}
`;

const StyledTableHeaderCell = styled.th<{ $isNumeric?: boolean; $isSortable?: boolean }>`
  text-align: left;
  font-weight: 600;
  color: var(--color-text);
  vertical-align: middle;
  white-space: nowrap;

  ${({ $isNumeric }) =>
    $isNumeric &&
    css`
      text-align: right;
    `}

  ${({ $isSortable }) =>
    $isSortable &&
    css`
      cursor: pointer;
      user-select: none;

      &:hover {
        background: var(--color-surface-hover);
      }
    `}
`;

const SortIcon = styled.span<{ $direction: 'asc' | 'desc' | null }>`
  display: inline-flex;
  margin-left: var(--space-xs);
  opacity: ${({ $direction }) => ($direction ? 1 : 0.3)};

  svg {
    width: 14px;
    height: 14px;
  }
`;

const sortAscIcon = (
  <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Sort Ascending</title>
    <path d='M12 5v14M5 12l7-7 7 7' />
  </svg>
);

const sortDescIcon = (
  <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Sort Descending</title>
    <path d='M12 5v14M19 12l-7 7-7-7' />
  </svg>
);

const sortNeutralIcon = (
  <svg viewBox='0 0 24 24' fill='none' stroke='currentColor' strokeWidth='2' aria-hidden='true'>
    <title>Sortable</title>
    <path d='M12 5v14M8 9l4-4 4 4M8 15l4 4 4-4' />
  </svg>
);

const getSortIcon = (direction: 'asc' | 'desc' | null) => {
  if (direction === 'asc') return sortAscIcon;
  if (direction === 'desc') return sortDescIcon;
  return sortNeutralIcon;
};

/** Renders a data table inside a horizontally scrollable container. */
export function Table({
  variant = 'simple',
  size = 'md',
  children,
  ...props
}: TableProps & ComponentProps<typeof Box>) {
  return (
    <TableContainer {...props}>
      <StyledTable $variant={variant} $size={size}>
        {children}
      </StyledTable>
    </TableContainer>
  );
}

/** Renders the thead section of a Table. */
export function TableHead({ children, className }: TableHeadProps) {
  return <StyledTableHead className={className}>{children}</StyledTableHead>;
}

/** Renders the tbody section of a Table. */
export function TableBody({ children, className }: TableBodyProps) {
  return <StyledTableBody className={className}>{children}</StyledTableBody>;
}

/** Renders the tfoot section of a Table. */
export function TableFoot({ children, className }: TableFootProps) {
  return <StyledTableFoot className={className}>{children}</StyledTableFoot>;
}

/** Renders a table row with hover highlighting and optional selected state. */
export function TableRow({ children, className, isSelected }: TableRowProps) {
  return (
    <StyledTableRow className={className} $isSelected={isSelected}>
      {children}
    </StyledTableRow>
  );
}

/** Renders a table data cell with optional right-aligned numeric formatting. */
export function TableCell({ children, isNumeric, ...props }: TableCellProps) {
  return (
    <StyledTableCell $isNumeric={isNumeric} {...props}>
      {children}
    </StyledTableCell>
  );
}

/** Renders a table header cell with optional sort direction indicator. */
export function TableHeaderCell({
  children,
  isNumeric,
  isSortable,
  sortDirection,
  onSort,
  ...props
}: TableHeaderCellProps) {
  return (
    <StyledTableHeaderCell
      $isNumeric={isNumeric}
      $isSortable={isSortable}
      onClick={isSortable ? onSort : undefined}
      {...props}
    >
      {children}
      {!!isSortable && <SortIcon $direction={sortDirection || null}>{getSortIcon(sortDirection || null)}</SortIcon>}
    </StyledTableHeaderCell>
  );
}

// Convenience aliases
/** Alias for TableHead. */
export const Thead = TableHead;
/** Alias for TableBody. */
export const Tbody = TableBody;
/** Alias for TableFoot. */
export const Tfoot = TableFoot;
/** Alias for TableRow. */
export const Tr = TableRow;
/** Alias for TableCell. */
export const Td = TableCell;
/** Alias for TableHeaderCell. */
export const Th = TableHeaderCell;
