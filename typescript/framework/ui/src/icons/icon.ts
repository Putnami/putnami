import { createElement, type ForwardRefExoticComponent, forwardRef, type RefAttributes, type SVGProps } from 'react';

type IconNode = [string, Record<string, string | number>];

export type IconProps = Omit<SVGProps<SVGSVGElement>, 'children'> & {
  color?: string;
  size?: string | number;
  strokeWidth?: string | number;
  absoluteStrokeWidth?: boolean;
};

export type Icon = ForwardRefExoticComponent<IconProps & RefAttributes<SVGSVGElement>>;
export type LucideIcon = Icon;

const defaultAttrs = {
  xmlns: 'http://www.w3.org/2000/svg',
  fill: 'none',
  viewBox: '0 0 24 24',
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
};

export function createIcon(name: string, nodes: IconNode[]): Icon {
  const Component = forwardRef<SVGSVGElement, IconProps>(
    ({ color = 'currentColor', size = 24, strokeWidth = 2, absoluteStrokeWidth, ...rest }, ref) =>
      createElement(
        'svg',
        {
          ref,
          ...defaultAttrs,
          width: size,
          height: size,
          stroke: color,
          strokeWidth: absoluteStrokeWidth ? (Number(strokeWidth) * 24) / Number(size) : strokeWidth,
          'aria-hidden': true,
          ...rest,
        },
        nodes.map(([tag, attrs], i) => createElement(tag, { ...attrs, key: i })),
      ),
  );
  Component.displayName = name;
  return Component;
}
