import type { MouseEvent, MouseEventHandler, ReactElement } from 'react';

type ClickableElement = ReactElement<{
  onClick?: MouseEventHandler<HTMLButtonElement>;
}>;

type ForwardRefRoot<P> = {
  render: (props: P, ref: unknown) => ClickableElement;
};

export const renderForwardRefRoot = <P>(component: unknown, props: P): ClickableElement =>
  (component as ForwardRefRoot<P>).render(props, null);

export const clickButtonElement = (element: ClickableElement): void => {
  let defaultPrevented = false;
  const event = {
    get defaultPrevented() {
      return defaultPrevented;
    },
    preventDefault: () => {
      defaultPrevented = true;
    },
  } as MouseEvent<HTMLButtonElement>;

  element.props.onClick?.(event);
};
