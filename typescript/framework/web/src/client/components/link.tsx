import React from 'react';
import { Link as RouterLink, type LinkProps as RouterLinkProps, useLocation } from 'react-router';
import { type PrefetchBehavior, usePrefetch } from '../hooks/use-prefetch.hook';

export type LinkProps = RouterLinkProps & { prefetch?: PrefetchBehavior };

export const Link = React.forwardRef<HTMLAnchorElement, LinkProps>(({ to, prefetch = 'none', ...props }, ref) => {
  const { load } = usePrefetch(to, prefetch);
  const location = useLocation();

  if (to === '#') {
    to = location.pathname;
  }

  if (!to) {
    to = location.pathname;
  }

  const targetPath = typeof to === 'string' ? to : to?.pathname;
  const isSelfTarget = targetPath === location.pathname;
  const mustPreload = prefetch === 'intent' && !to.toString().startsWith('http');

  const handleClick = (e: React.MouseEvent<HTMLAnchorElement>) => {
    if (isSelfTarget) {
      e.preventDefault();
      return;
    }
    props.onClick?.(e);
  };

  const handleMouseEnter = (e: React.MouseEvent<HTMLAnchorElement>) => {
    if (mustPreload) {
      load();
    }
    props.onMouseEnter?.(e);
  };

  const handleFocus = (e: React.FocusEvent<HTMLAnchorElement>) => {
    if (mustPreload) {
      load();
    }
    props.onFocus?.(e);
  };

  const handleTouchStart = (e: React.TouchEvent<HTMLAnchorElement>) => {
    if (mustPreload) {
      load();
    }
    props.onTouchStart?.(e);
  };
  return (
    <RouterLink
      {...props}
      ref={ref}
      to={to}
      onClick={handleClick}
      onMouseEnter={handleMouseEnter}
      onFocus={handleFocus}
      onTouchStart={handleTouchStart}
    />
  );
});

Link.displayName = 'Link';
