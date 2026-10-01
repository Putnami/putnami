import type React from 'react';
import type { ActionDefinition } from './action';
import type { ErrorDefinition } from './error';
import type { LayoutDefinition } from './layout';
import type { LoaderDefinition } from './loader';
import type { NotFoundDefinition } from './not-found';
import type { PageDefinition } from './page';

export interface PageModule {
  page?: React.ComponentType | PageDefinition;
  default?: React.ComponentType | PageDefinition;
}

export interface LayoutModule {
  layout?: React.ComponentType | LayoutDefinition;
  default?: React.ComponentType | LayoutDefinition;
}

export interface ErrorModule {
  error?: React.ComponentType | ErrorDefinition;
  default?: React.ComponentType | ErrorDefinition;
}

export interface NotFoundModule {
  notFound?: React.ComponentType | NotFoundDefinition;
  default?: React.ComponentType | NotFoundDefinition;
}

export interface LoaderModule {
  loader?: LoaderDefinition;
  default?: LoaderDefinition;
}

export interface ActionModule {
  action?: ActionDefinition;
  default?: ActionDefinition;
}

// Lazy module loaders - these defer loading until first request
export type LazyPageModule = () => Promise<PageModule>;
export type LazyLayoutModule = () => Promise<LayoutModule>;
export type LazyErrorModule = () => Promise<ErrorModule>;
export type LazyNotFoundModule = () => Promise<NotFoundModule>;
export type LazyLoaderModule = () => Promise<LoaderModule>;
export type LazyActionModule = () => Promise<ActionModule>;
