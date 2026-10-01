export interface BaseRouteNode {
  path?: string;
  id?: string;
  index?: boolean;
  element?: unknown;
  loader?: unknown;
  action?: unknown;
  children?: BaseRouteNode[];
  error?: string;
}

export interface ClientRouteNode extends BaseRouteNode {
  element?: string;
  loader?: string;
  action?: string;
  isLayout?: boolean;
  notFound?: string;
  children?: ClientRouteNode[];
}
