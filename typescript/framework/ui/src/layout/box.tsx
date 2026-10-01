/** @jsxImportSource @emotion/react */
import type { Interpolation, Theme } from '@emotion/react';
import { styled } from '../emotion';
import { getColor, getSpacing, getThemeValue } from '../theme';
import { responsiveCss } from './responsive';

/** Style props for the Box component supporting responsive values for margin, padding, sizing, positioning, and colors. */
export type BoxProps = {
  m?: string | number | (string | number)[];
  mt?: string | number | (string | number)[];
  mr?: string | number | (string | number)[];
  mb?: string | number | (string | number)[];
  ml?: string | number | (string | number)[];
  mx?: string | number | (string | number)[];
  my?: string | number | (string | number)[];
  p?: string | number | (string | number)[];
  pt?: string | number | (string | number)[];
  pr?: string | number | (string | number)[];
  pb?: string | number | (string | number)[];
  pl?: string | number | (string | number)[];
  px?: string | number | (string | number)[];
  py?: string | number | (string | number)[];
  width?: string | number | (string | number)[];
  height?: string | number | (string | number)[];
  maxWidth?: string | number | (string | number)[];
  minWidth?: string | number | (string | number)[];
  bg?: string;
  color?: string;
  display?: string | string[];
  textAlign?: string | string[];
  fontSize?: string | number | (string | number)[];
  overflow?: string | string[];
  position?: string | string[];
  top?: string | number | (string | number)[];
  right?: string | number | (string | number)[];
  bottom?: string | number | (string | number)[];
  left?: string | number | (string | number)[];
  zIndex?: number | string;
  border?: string;
  borderRadius?: string | number;
  flex?: string | number;
  flexGrow?: string | number;
  flexShrink?: string | number;
  viewBox?: string;
  fill?: string;
  css?: Interpolation<Theme>;
  type?: string;
  alignItems?: string | string[];
  justifyContent?: string | string[];
};

/** Base layout primitive. A styled div that maps props to CSS with responsive breakpoint support and theme-aware spacing/colors. */
export const Box = styled.div<BoxProps>`
  box-sizing: border-box;
  ${({ theme, m }: BoxProps & { theme: Theme }) => m && responsiveCss(theme, 'margin', m, (v) => getSpacing(theme, v))}
  ${({ theme, mt }: BoxProps & { theme: Theme }) => mt && responsiveCss(theme, 'margin-top', mt, (v) => getSpacing(theme, v))}
  ${({ theme, mr }: BoxProps & { theme: Theme }) => mr && responsiveCss(theme, 'margin-right', mr, (v) => getSpacing(theme, v))}
  ${({ theme, mb }: BoxProps & { theme: Theme }) => mb && responsiveCss(theme, 'margin-bottom', mb, (v) => getSpacing(theme, v))}
  ${({ theme, ml }: BoxProps & { theme: Theme }) => ml && responsiveCss(theme, 'margin-left', ml, (v) => getSpacing(theme, v))}
  ${({ theme, mx }: BoxProps & { theme: Theme }) => mx && responsiveCss(theme, 'margin-inline', mx, (v) => getSpacing(theme, v))}
  ${({ theme, my }: BoxProps & { theme: Theme }) => my && responsiveCss(theme, 'margin-block', my, (v) => getSpacing(theme, v))}

  ${({ theme, p }: BoxProps & { theme: Theme }) => p && responsiveCss(theme, 'padding', p, (v) => getSpacing(theme, v))}
  ${({ theme, pt }: BoxProps & { theme: Theme }) => pt && responsiveCss(theme, 'padding-top', pt, (v) => getSpacing(theme, v))}
  ${({ theme, pr }: BoxProps & { theme: Theme }) => pr && responsiveCss(theme, 'padding-right', pr, (v) => getSpacing(theme, v))}
  ${({ theme, pb }: BoxProps & { theme: Theme }) => pb && responsiveCss(theme, 'padding-bottom', pb, (v) => getSpacing(theme, v))}
  ${({ theme, pl }: BoxProps & { theme: Theme }) => pl && responsiveCss(theme, 'padding-left', pl, (v) => getSpacing(theme, v))}
  ${({ theme, px }: BoxProps & { theme: Theme }) => px && responsiveCss(theme, 'padding-inline', px, (v) => getSpacing(theme, v))}
  ${({ theme, py }: BoxProps & { theme: Theme }) => py && responsiveCss(theme, 'padding-block', py, (v) => getSpacing(theme, v))}

  ${({ theme, width }: BoxProps & { theme: Theme }) => width && responsiveCss(theme, 'width', width)}
  ${({ theme, height }: BoxProps & { theme: Theme }) => height && responsiveCss(theme, 'height', height)}
  ${({ theme, maxWidth }: BoxProps & { theme: Theme }) => maxWidth && responsiveCss(theme, 'max-width', maxWidth)}
  ${({ theme, minWidth }: BoxProps & { theme: Theme }) => minWidth && responsiveCss(theme, 'min-width', minWidth)}
  ${({ theme, display }: BoxProps & { theme: Theme }) => display && responsiveCss(theme, 'display', display)}
  ${({ theme, textAlign }: BoxProps & { theme: Theme }) => textAlign && responsiveCss(theme, 'text-align', textAlign)}
  ${({ theme, fontSize }: BoxProps & { theme: Theme }) => fontSize && responsiveCss(theme, 'font-size', fontSize)}
  ${({ theme, alignItems }: BoxProps & { theme: Theme }) => alignItems && responsiveCss(theme, 'align-items', alignItems)}
  ${({ theme, justifyContent }: BoxProps & { theme: Theme }) => justifyContent && responsiveCss(theme, 'justify-content', justifyContent)}
  ${({ theme, overflow }: BoxProps & { theme: Theme }) => overflow && responsiveCss(theme, 'overflow', overflow)}

  ${({ theme, position }: BoxProps & { theme: Theme }) => position && responsiveCss(theme, 'position', position)}
  ${({ theme, top }: BoxProps & { theme: Theme }) => top && responsiveCss(theme, 'top', top)}
  ${({ theme, right }: BoxProps & { theme: Theme }) => right && responsiveCss(theme, 'right', right)}
  ${({ theme, bottom }: BoxProps & { theme: Theme }) => bottom && responsiveCss(theme, 'bottom', bottom)}
  ${({ theme, left }: BoxProps & { theme: Theme }) => left && responsiveCss(theme, 'left', left)}
  ${({ zIndex }: BoxProps) => zIndex !== undefined && `z-index: ${zIndex};`}

  ${({ theme, bg }: BoxProps & { theme: Theme }) => bg && `background-color: ${getColor(theme, bg)};`}
  ${({ theme, color }: BoxProps & { theme: Theme }) => color && `color: ${getColor(theme, color)};`}

  ${({ border }: BoxProps) => border && `border: ${border};`}
  ${({ theme, borderRadius }: BoxProps & { theme: Theme }) => borderRadius && `border-radius: ${getThemeValue(theme.radii, String(borderRadius)) || borderRadius};`}

  ${({ flex }: BoxProps) => flex !== undefined && `flex: ${flex};`}
  ${({ flexGrow }: BoxProps) => flexGrow !== undefined && `flex-grow: ${flexGrow};`}
  ${({ flexShrink }: BoxProps) => flexShrink !== undefined && `flex-shrink: ${flexShrink};`}
`;
