/** @jsxImportSource @emotion/react */
import type { Theme } from '@emotion/react';
import { styled } from '../emotion';
import { getSpacing } from '../theme';
import { Box, type BoxProps } from './box';
import { responsiveCss } from './responsive';

/** Props extending BoxProps with flexbox-specific properties (gap, direction, align, justify, wrap). */
export type FlexProps = BoxProps & {
  gap?: string | number | (string | number)[];
  direction?: string | string[];
  align?: string | string[];
  justify?: string | string[];
  wrap?: string | string[];
  flexDirection?: string | string[];
  alignItems?: string | string[];
  justifyContent?: string | string[];
  flexWrap?: string | string[];
};

/** Flexbox layout component. Extends Box with `display: flex` and flexbox shorthand props. */
export const Flex = styled(Box)<FlexProps>`
  ${({ display }: FlexProps & { theme: Theme }) => !display && 'display: flex;'}
  ${({ theme, gap }: FlexProps & { theme: Theme }) =>
    gap && responsiveCss(theme, 'gap', gap, (v) => getSpacing(theme, v))}

  ${({ theme, direction }: FlexProps & { theme: Theme }) => direction && responsiveCss(theme, 'flex-direction', direction)}
  ${({ theme, flexDirection }: FlexProps & { theme: Theme }) =>
    flexDirection && responsiveCss(theme, 'flex-direction', flexDirection)}

  ${({ theme, align }: FlexProps & { theme: Theme }) => align && responsiveCss(theme, 'align-items', align)}
  ${({ theme, alignItems }: FlexProps & { theme: Theme }) => alignItems && responsiveCss(theme, 'align-items', alignItems)}

  ${({ theme, justify }: FlexProps & { theme: Theme }) => justify && responsiveCss(theme, 'justify-content', justify)}
  ${({ theme, justifyContent }: FlexProps & { theme: Theme }) =>
    justifyContent && responsiveCss(theme, 'justify-content', justifyContent)}

  ${({ theme, wrap }: FlexProps & { theme: Theme }) => wrap && responsiveCss(theme, 'flex-wrap', wrap)}
  ${({ theme, flexWrap }: FlexProps & { theme: Theme }) => flexWrap && responsiveCss(theme, 'flex-wrap', flexWrap)}
`;
