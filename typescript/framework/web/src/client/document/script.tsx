import type { ScriptHTMLAttributes } from 'react';
import { documentHelper } from './document.helper';

type ScriptAttr = ScriptHTMLAttributes<HTMLScriptElement>;

export const Script = (attr: ScriptAttr) => {
  documentHelper().addScript(attr);
  return null;
};
