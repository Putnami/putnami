import type { ComponentPropsWithoutRef, ReactNode } from 'react';
import { forwardRef } from 'react';
import { css, styled } from '../emotion';

type AvatarSize = 'xs' | 'sm' | 'md' | 'lg' | 'xl' | '2xl';

/** Props for the Avatar component, a circular user representation showing an image, initials, or fallback icon. */
export interface AvatarProps extends ComponentPropsWithoutRef<'div'> {
  src?: string;
  name?: string;
  size?: AvatarSize;
  icon?: ReactNode;
}

const sizeStyles: Record<AvatarSize, { size: string; fontSize: string }> = {
  xs: { size: '24px', fontSize: '0.6rem' },
  sm: { size: '32px', fontSize: '0.75rem' },
  md: { size: '40px', fontSize: '0.875rem' },
  lg: { size: '48px', fontSize: '1rem' },
  xl: { size: '64px', fontSize: '1.25rem' },
  '2xl': { size: '96px', fontSize: '1.75rem' },
};

const getInitials = (name: string): string => {
  const parts = name.trim().split(/\s+/);
  if (parts.length === 1) {
    return parts[0].charAt(0).toUpperCase();
  }
  return (parts[0].charAt(0) + parts[parts.length - 1].charAt(0)).toUpperCase();
};

const getColorFromName = (name: string): string => {
  const colors = [
    '#6366f1', // indigo
    '#8b5cf6', // violet
    '#a855f7', // purple
    '#d946ef', // fuchsia
    '#ec4899', // pink
    '#f43f5e', // rose
    '#ef4444', // red
    '#f97316', // orange
    '#f59e0b', // amber
    '#eab308', // yellow
    '#84cc16', // lime
    '#22c55e', // green
    '#10b981', // emerald
    '#14b8a6', // teal
    '#06b6d4', // cyan
    '#0ea5e9', // sky
    '#3b82f6', // blue
  ];

  let hash = 0;
  for (let i = 0; i < name.length; i++) {
    hash = name.charCodeAt(i) + ((hash << 5) - hash);
  }
  return colors[Math.abs(hash) % colors.length];
};

const StyledAvatar = styled.div<{
  $size: AvatarSize;
  $bgColor?: string;
}>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  overflow: hidden;
  flex-shrink: 0;
  font-family: var(--font-sans);
  font-weight: 500;
  color: white;
  background: ${({ $bgColor }) => $bgColor || 'var(--color-surface-hover)'};

  ${({ $size }) => css`
    width: ${sizeStyles[$size].size};
    height: ${sizeStyles[$size].size};
    font-size: ${sizeStyles[$size].fontSize};
  `}
`;

const AvatarImage = styled.img`
  width: 100%;
  height: 100%;
  object-fit: cover;
`;

const IconWrapper = styled.span<{ $size: AvatarSize }>`
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--color-text-muted);

  ${({ $size }) => css`
    width: calc(${sizeStyles[$size].size} * 0.6);
    height: calc(${sizeStyles[$size].size} * 0.6);
  `}

  svg {
    width: 100%;
    height: 100%;
  }
`;

const defaultIcon = (
  <svg viewBox='0 0 24 24' fill='currentColor' aria-hidden='true'>
    <title>Avatar</title>
    <path d='M12 12c2.21 0 4-1.79 4-4s-1.79-4-4-4-4 1.79-4 4 1.79 4 4 4zm0 2c-2.67 0-8 1.34-8 4v2h16v-2c0-2.66-5.33-4-8-4z' />
  </svg>
);

/** Renders a circular avatar displaying an image, name initials, or a fallback icon. */
export const Avatar = forwardRef<HTMLDivElement, AvatarProps>(function Avatar(
  { src, name, size = 'md', icon, ...rest },
  ref,
) {
  const bgColor = name ? getColorFromName(name) : undefined;

  return (
    <StyledAvatar ref={ref} $size={size} $bgColor={bgColor} role='img' aria-label={name} {...rest}>
      {src ? (
        <AvatarImage src={src} alt={name || 'Avatar'} />
      ) : name ? (
        getInitials(name)
      ) : (
        <IconWrapper $size={size}>{icon || defaultIcon}</IconWrapper>
      )}
    </StyledAvatar>
  );
});

// Avatar Group
/** Props for the AvatarGroup component, which displays a stack of avatars with an optional overflow count. */
export interface AvatarGroupProps extends ComponentPropsWithoutRef<'div'> {
  max?: number;
  size?: AvatarSize;
  spacing?: string;
  children: ReactNode;
}

const StyledAvatarGroup = styled.div<{ $spacing: string }>`
  display: inline-flex;
  flex-direction: row-reverse;
  justify-content: flex-end;

  > * {
    border: 2px solid var(--color-bg);
    margin-left: ${({ $spacing }) => $spacing};

    &:last-child {
      margin-left: 0;
    }
  }
`;

const ExcessLabel = styled.div<{ $size: AvatarSize }>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  background: var(--color-surface-hover);
  color: var(--color-text);
  font-family: var(--font-sans);
  font-weight: 500;
  border: 2px solid var(--color-bg);

  ${({ $size }) => css`
    width: ${sizeStyles[$size].size};
    height: ${sizeStyles[$size].size};
    font-size: ${sizeStyles[$size].fontSize};
  `}
`;

/** Renders a group of overlapping avatars with an optional "+N" excess indicator. */
export const AvatarGroup = forwardRef<HTMLDivElement, AvatarGroupProps>(function AvatarGroup(
  { max, size = 'md', spacing = '-8px', children, ...rest },
  ref,
) {
  const childArray = Array.isArray(children) ? children : [children];
  const excess = max && childArray.length > max ? childArray.length - max : 0;
  const visibleChildren = max ? childArray.slice(0, max) : childArray;

  return (
    <StyledAvatarGroup ref={ref} $spacing={spacing} {...rest}>
      {excess > 0 && <ExcessLabel $size={size}>+{excess}</ExcessLabel>}
      {[...visibleChildren].reverse()}
    </StyledAvatarGroup>
  );
});
