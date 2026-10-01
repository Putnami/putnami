import React from 'react';
import { describe, expect, it } from 'bun:test';
import { error } from '../../src/ssr/error';
import { layout } from '../../src/ssr/layout';
import { notFound } from '../../src/ssr/not-found';
import { page } from '../../src/ssr/page';
import {
  resolveErrorOrDefinition,
  resolveLayoutOrDefinition,
  resolveNotFoundOrDefinition,
  resolvePageOrDefinition,
} from '../../src/ssr/route-module.utils';

const PageComponent = () => React.createElement('div', null, 'page');
const LayoutComponent = () => React.createElement('section', null, 'layout');
const ErrorComponent = () => React.createElement('div', null, 'error');
const NotFoundComponent = () => React.createElement('div', null, 'not-found');

describe('route-module.utils', () => {
  it('resolves undefined page modules', () => {
    expect(resolvePageOrDefinition(undefined)).toEqual({
      page: undefined,
      pageDef: undefined,
    });
  });

  it('resolves plain page modules and page definitions', () => {
    const plain = resolvePageOrDefinition({ default: PageComponent });
    const defined = resolvePageOrDefinition({ default: page().status(201).render(PageComponent) });

    expect(plain.pageDef).toBeUndefined();
    expect(plain.page).toBeDefined();
    expect(defined.pageDef?.statusCode).toBe(201);
    expect(defined.page).toBeDefined();
  });

  it('passes through direct page elements', () => {
    const element = React.createElement('div', null, 'direct');
    expect(resolvePageOrDefinition(element)).toEqual({
      page: element,
      pageDef: undefined,
    });
  });

  it('resolves layout modules and layout definitions', () => {
    const plain = resolveLayoutOrDefinition({ default: LayoutComponent });
    const defined = resolveLayoutOrDefinition({ default: layout().render(LayoutComponent) });

    expect(plain.layoutDef).toBeUndefined();
    expect(plain.element).toBeDefined();
    expect(defined.layoutDef).toBeDefined();
    expect(defined.element).toBeDefined();
  });

  it('resolves error modules and error definitions', () => {
    const plain = resolveErrorOrDefinition({ default: ErrorComponent });
    const defined = resolveErrorOrDefinition({ default: error().status(500).render(ErrorComponent) });

    expect(plain.errorDef).toBeUndefined();
    expect(defined.errorDef?.statusCode).toBe(500);
    expect(defined.element).toBeDefined();
  });

  it('resolves not-found modules and definitions', () => {
    const plain = resolveNotFoundOrDefinition({ default: NotFoundComponent });
    const defined = resolveNotFoundOrDefinition({ default: notFound().render(NotFoundComponent) });

    expect(plain.notFoundDef).toBeUndefined();
    expect(plain.element).toBeDefined();
    expect(defined.notFoundDef).toBeDefined();
    expect(defined.element).toBeDefined();
  });
});
