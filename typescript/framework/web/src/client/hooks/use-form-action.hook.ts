import { useFormAction as _useFormAction } from 'react-router';

export const useFormAction = (action?: string, opts?: { relative?: 'route' | 'path' }): string =>
  _useFormAction(action, opts);
