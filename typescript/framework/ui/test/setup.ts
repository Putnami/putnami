import { mock } from 'bun:test';
import React, { type ComponentPropsWithoutRef } from 'react';
import * as reactDomOrig from 'react-dom';

type MockLinkProps = Omit<ComponentPropsWithoutRef<'a'>, 'href'> & {
  to: string;
};

interface MockDocumentMeta {
  scripts?: Array<{ children?: unknown }>;
  styleExtractors?: Array<() => unknown[]>;
}

let mockDocumentMeta: MockDocumentMeta | null = null;

export function setMockDocumentMeta(value: MockDocumentMeta | null) {
  mockDocumentMeta = value;
}

globalThis.document = {
  body: {},
  addEventListener: () => {},
  removeEventListener: () => {},
  querySelectorAll: () => [],
} as unknown as Document;

mock.module('react-dom', () => ({
  ...reactDomOrig,
  createPortal: (node: React.ReactNode) => node,
}));

mock.module('@putnami/web', () => ({
  Link: ({ to, children, className, ...props }: MockLinkProps) =>
    React.createElement('a', { href: to, className, ...props }, children),
  useNavigate: () => () => {},
  useDocumentMeta: () => mockDocumentMeta,
}));
