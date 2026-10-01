import type { LinkHTMLAttributes } from 'react';
import { documentHelper } from './document.helper';

type LinkAttr = LinkHTMLAttributes<HTMLLinkElement>;

export const HeaderLink = (attr: LinkAttr) => {
  documentHelper().addLink(attr);

  return null;
};

type FaviconType = 'image/svg+xml' | 'image/png' | 'image/x-icon';
type FaviconAttr = {
  type?: FaviconType;
  href?: string;
};
export const Favicon = ({ href = '/favicon.ico' }: FaviconAttr = {}) => {
  let type = 'image/x-icon';
  if (href.endsWith('.png')) {
    type = 'image/png';
  } else if (href.endsWith('.svg')) {
    type = 'image/svg+xml';
  }

  return HeaderLink({
    rel: 'icon',
    type,
    href,
  });
};
