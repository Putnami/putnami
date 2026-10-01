import { documentHelper } from './document.helper';

export const Lang = ({ children, lang }: { children?: string; lang?: string }) => {
  if (typeof children === 'string') {
    documentHelper().lang = children;
  } else if (typeof lang === 'string') {
    documentHelper().lang = lang;
  }

  return null;
};
