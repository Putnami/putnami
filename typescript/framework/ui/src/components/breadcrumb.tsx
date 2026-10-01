import type { ReactNode } from 'react';
import { Children, isValidElement } from 'react';
import { css, styled } from '../emotion';

/** Props for the Breadcrumb component, a navigation trail showing page hierarchy. */
export interface BreadcrumbProps {
  separator?: ReactNode;
  spacing?: string;
  children?: ReactNode;
  className?: string;
}

/** Props for an individual breadcrumb item wrapper. */
export interface BreadcrumbItemProps {
  isCurrentPage?: boolean;
  children?: ReactNode;
  className?: string;
}

/** Props for a breadcrumb link, with current-page styling when active. */
export interface BreadcrumbLinkProps {
  href?: string;
  isCurrentPage?: boolean;
  children?: ReactNode;
  className?: string;
  onClick?: () => void;
}

const StyledBreadcrumb = styled.nav`
  font-family: var(--font-sans);
  font-size: 0.9rem;
`;

const BreadcrumbList = styled.ol<{ $spacing: string }>`
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  list-style: none;
  margin: 0;
  padding: 0;
  gap: ${({ $spacing }) => $spacing};
`;

const StyledBreadcrumbItem = styled.li`
  display: flex;
  align-items: center;
`;

const Separator = styled.span`
  display: flex;
  align-items: center;
  color: var(--color-text-muted);
  user-select: none;
`;

const StyledBreadcrumbLink = styled.a<{ $isCurrentPage?: boolean }>`
  color: var(--color-text-muted);
  text-decoration: none;
  transition: color var(--transition-fast);

  ${({ $isCurrentPage }) =>
    $isCurrentPage
      ? css`
          color: var(--color-text);
          font-weight: 500;
          cursor: default;
          pointer-events: none;
        `
      : css`
          &:hover {
            color: var(--color-text);
            text-decoration: underline;
          }
        `}
`;

const defaultSeparator = (
  <svg
    width='16'
    height='16'
    viewBox='0 0 24 24'
    fill='none'
    stroke='currentColor'
    strokeWidth='2'
    strokeLinecap='round'
    strokeLinejoin='round'
    aria-hidden='true'
  >
    <title>Separator</title>
    <polyline points='9 18 15 12 9 6' />
  </svg>
);

/** Renders a breadcrumb navigation trail with separator icons between items. */
export function Breadcrumb({ separator = defaultSeparator, spacing = 'var(--space-xs)', children }: BreadcrumbProps) {
  const items = Children.toArray(children).filter(isValidElement);

  return (
    <StyledBreadcrumb aria-label='Breadcrumb'>
      <BreadcrumbList $spacing={spacing}>
        {items.map((child, index) => {
          const itemKey = isValidElement(child) && child.key !== null ? String(child.key) : `crumb-${index}`;
          return (
            <StyledBreadcrumbItem key={itemKey}>
              {child}
              {index < items.length - 1 ? <Separator>{separator}</Separator> : null}
            </StyledBreadcrumbItem>
          );
        })}
      </BreadcrumbList>
    </StyledBreadcrumb>
  );
}

/** Renders a breadcrumb item wrapper. */
export function BreadcrumbItem({ children, className }: BreadcrumbItemProps) {
  return <span className={className}>{children}</span>;
}

/** Renders a breadcrumb link that becomes non-interactive when marking the current page. */
export function BreadcrumbLink({ href, isCurrentPage, children, className, onClick }: BreadcrumbLinkProps) {
  const ariaCurrent = isCurrentPage ? ('page' as const) : undefined;
  return (
    <StyledBreadcrumbLink
      href={isCurrentPage ? undefined : (href ?? '')}
      $isCurrentPage={isCurrentPage}
      className={className}
      onClick={onClick}
      aria-current={ariaCurrent}
    >
      {children}
    </StyledBreadcrumbLink>
  );
}
