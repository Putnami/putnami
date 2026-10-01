import type { StyleHTMLAttributes } from 'react';
import { documentHelper } from './document.helper';

type StyleAttr = StyleHTMLAttributes<HTMLStyleElement>;

export const Style = (attr: StyleAttr) => {
  documentHelper().addStyle(attr);
  return null;
};
