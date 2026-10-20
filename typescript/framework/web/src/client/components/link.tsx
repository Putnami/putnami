import React from 'react';
import {
  createPath,
  Link as RouterLink,
  type LinkProps as RouterLinkProps,
  useInRouterContext,
  useLocation,
} from 'react-router';
import { type PrefetchBehavior, usePrefetch } from '../hooks/use-prefetch.hook';

export type LinkProps = RouterLinkProps & { prefetch?: PrefetchBehavior };

/**
 * A link to the current location, including one that changes only the query or
 * the hash, navigates like any other: React Router replaces the entry and
 * re-runs the loaders of a same-URL navigation.
 *
 * Outside a router (a static render, a component test), the link renders a
 * plain `<a href>` with the same anchor props.
 */
export const Link = React.forwardRef<HTMLAnchorElement, LinkProps>((props, ref) =>
  useInRouterContext() ? <RoutedLink {...props} ref={ref} /> : <PlainLink {...props} ref={ref} />,
);

Link.displayName = 'Link';

const RoutedLink = React.forwardRef<HTMLAnchorElement, LinkProps>(({ to, prefetch = 'none', ...props }, ref) => {
  const { load } = usePrefetch(to, prefetch);
  const location = useLocation();

  // `to="#"` and an empty `to` mark a placeholder link: it points at the
  // current path, runs its own `onClick`, and does not navigate, because a
  // navigation would drop the current query and hash and re-run the loaders.
  const isPlaceholder = to === '#' || !to;
  if (isPlaceholder) {
    to = location.pathname;
  }

  const mustPreload = prefetch === 'intent' && !to.toString().startsWith('http');

  const handleClick = (e: React.MouseEvent<HTMLAnchorElement>) => {
    props.onClick?.(e);
    if (isPlaceholder) {
      e.preventDefault();
    }
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

RoutedLink.displayName = 'RoutedLink';

const PlainLink = React.forwardRef<HTMLAnchorElement, LinkProps>(
  (
    {
      to,
      prefetch: _prefetch,
      discover: _discover,
      reloadDocument: _reloadDocument,
      replace: _replace,
      state: _state,
      preventScrollReset: _preventScrollReset,
      relative: _relative,
      viewTransition: _viewTransition,
      defaultShouldRevalidate: _defaultShouldRevalidate,
      mask: _mask,
      ...anchorProps
    },
    ref,
  ) => <a {...anchorProps} ref={ref} href={typeof to === 'string' ? to : createPath(to)} />,
);

PlainLink.displayName = 'PlainLink';
