import { HttpResponse } from './http-response';

export function redirect(uri: string, status: 300 | 301 | 302 | 303 | 304 | 307 | 308 = 302): never {
  throw HttpResponse.redirect(uri, status);
}
