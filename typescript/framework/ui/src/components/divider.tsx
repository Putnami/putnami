import { css, styled } from '../emotion';

type DividerOrientation = 'horizontal' | 'vertical';
type DividerVariant = 'solid' | 'dashed' | 'dotted';

/** Props for the Divider component, a horizontal or vertical separator line. */
export interface DividerProps {
  orientation?: DividerOrientation;
  variant?: DividerVariant;
  color?: string;
  thickness?: string;
  className?: string;
}

const StyledDivider = styled.hr<{
  $orientation: DividerOrientation;
  $variant: DividerVariant;
  $color?: string;
  $thickness: string;
}>`
  border: none;
  margin: 0;
  flex-shrink: 0;

  ${({ $orientation, $variant, $color, $thickness }) =>
    $orientation === 'horizontal'
      ? css`
          width: 100%;
          height: 0;
          border-top-width: ${$thickness};
          border-top-style: ${$variant};
          border-top-color: ${$color || 'var(--color-border)'};
        `
      : css`
          height: auto;
          align-self: stretch;
          width: 0;
          border-left-width: ${$thickness};
          border-left-style: ${$variant};
          border-left-color: ${$color || 'var(--color-border)'};
        `}
`;

/** Renders a horizontal or vertical separator line with configurable style and thickness. */
export function Divider({
  orientation = 'horizontal',
  variant = 'solid',
  color,
  thickness = '1px',
  className,
}: DividerProps) {
  return (
    <StyledDivider
      $orientation={orientation}
      $variant={variant}
      $color={color}
      $thickness={thickness}
      className={className}
      aria-orientation={orientation}
    />
  );
}
