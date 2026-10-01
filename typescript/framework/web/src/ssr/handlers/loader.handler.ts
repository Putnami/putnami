import type { HttpRequestContext } from '@putnami/application';
import { useContext } from '@putnami/runtime';

export const loaderHandler = (handler: (ctx: HttpRequestContext) => unknown) => () => handler(useContext());
