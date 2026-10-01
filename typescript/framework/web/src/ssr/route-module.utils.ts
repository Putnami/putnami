import React, { type ReactNode } from 'react';
import type { ErrorDefinition } from './error';
import { isErrorDefinition } from './error';
import type { LayoutDefinition } from './layout';
import { isLayoutDefinition } from './layout';
import type { ErrorModule, LayoutModule, NotFoundModule, PageModule } from './module.types';
import type { NotFoundDefinition } from './not-found';
import { isNotFoundDefinition } from './not-found';
import type { PageDefinition } from './page';
import { isPageDefinition } from './page';
import { createPageElement } from '../shared/page-element';
import { inModuleOrDefault } from './react-ssr.utils';

/**
 * Resolve the component of a page module: the module exports either a
 * PageDefinition or a plain ComponentType.
 */
export function resolvePageComponent(pageModule: PageModule): {
  component: React.ComponentType;
  pageDef: PageDefinition | undefined;
} {
  const def = inModuleOrDefault<React.ComponentType | PageDefinition, PageModule>(pageModule, 'page');
  if (isPageDefinition(def)) {
    return { component: def.component, pageDef: def };
  }
  return { component: def as React.ComponentType, pageDef: undefined };
}

/**
 * Resolve a page to the element its route renders. A page module resolves to
 * its component inside the page boundary; an element is returned as it is.
 */
export function resolvePageOrDefinition(page: ReactNode | PageModule | undefined): {
  page: ReactNode | undefined;
  pageDef: PageDefinition | undefined;
} {
  if (!page) return { page: undefined, pageDef: undefined };

  if (typeof page === 'object' && page !== null && ('default' in page || 'page' in page)) {
    const { component, pageDef } = resolvePageComponent(page as PageModule);
    return { page: createPageElement(component), pageDef };
  }

  return { page: page as ReactNode, pageDef: undefined };
}

/**
 * Resolve a layout module: either a LayoutDefinition or a plain ComponentType.
 */
export function resolveLayoutOrDefinition(layoutMod: LayoutModule | undefined): {
  element: ReactNode | undefined;
  layoutDef: LayoutDefinition | undefined;
} {
  if (!layoutMod) return { element: undefined, layoutDef: undefined };

  const def = inModuleOrDefault<React.ComponentType | LayoutDefinition, LayoutModule>(layoutMod, 'layout');
  if (isLayoutDefinition(def)) {
    return {
      element: React.createElement(def.component),
      layoutDef: def,
    };
  }
  return {
    element: React.createElement(def as React.ComponentType),
    layoutDef: undefined,
  };
}

/**
 * Resolve an error module: either an ErrorDefinition or a plain ComponentType.
 */
export function resolveErrorOrDefinition(errorMod: ErrorModule | undefined): {
  element: ReactNode | undefined;
  errorDef: ErrorDefinition | undefined;
} {
  if (!errorMod) return { element: undefined, errorDef: undefined };

  const def = inModuleOrDefault<React.ComponentType | ErrorDefinition, ErrorModule>(errorMod, 'error');
  if (isErrorDefinition(def)) {
    return {
      element: React.createElement(def.component),
      errorDef: def,
    };
  }
  return {
    element: React.createElement(def as React.ComponentType),
    errorDef: undefined,
  };
}

/**
 * Resolve a not-found module: either a NotFoundDefinition or a plain ComponentType.
 */
export function resolveNotFoundOrDefinition(notFoundMod: NotFoundModule | undefined): {
  element: ReactNode | undefined;
  notFoundDef: NotFoundDefinition | undefined;
} {
  if (!notFoundMod) return { element: undefined, notFoundDef: undefined };

  const def = inModuleOrDefault<React.ComponentType | NotFoundDefinition, NotFoundModule>(notFoundMod, 'notFound');
  if (isNotFoundDefinition(def)) {
    return {
      element: React.createElement(def.component),
      notFoundDef: def,
    };
  }
  return {
    element: React.createElement(def as React.ComponentType),
    notFoundDef: undefined,
  };
}
