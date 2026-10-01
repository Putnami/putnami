/** @jsxImportSource @emotion/react */
import { styled } from '../emotion';
import { getSpacing } from '../theme';
import { Box, type BoxProps } from './box';
import { responsiveCss } from './responsive';

/** Props extending BoxProps with CSS grid properties (gap, columns, rows, areas, autoFlow). */
export type GridProps = BoxProps & {
  gap?: string | number | (string | number)[];
  columns?: string | number | (string | number)[];
  rows?: string | number | (string | number)[];
  areas?: string | string[];
  autoFlow?: string | string[];
};

/** CSS Grid layout component. Number values for `columns` generate `repeat(n, minmax(0, 1fr))`. */
export const Grid = styled(Box)<GridProps>`
  display: grid;
  ${({ theme, gap }) => gap && responsiveCss(theme, 'gap', gap, (v) => getSpacing(theme, v))}
  ${({ theme, columns }) =>
    columns &&
    responsiveCss(theme, 'grid-template-columns', columns, (v) =>
      typeof v === 'number' ? `repeat(${v}, minmax(0, 1fr))` : v,
    )}
  ${({ theme, rows }) => rows && responsiveCss(theme, 'grid-template-rows', rows)}
  ${({ theme, areas }) => areas && responsiveCss(theme, 'grid-template-areas', areas)}
  ${({ theme, autoFlow }) => autoFlow && responsiveCss(theme, 'grid-auto-flow', autoFlow)}
`;
