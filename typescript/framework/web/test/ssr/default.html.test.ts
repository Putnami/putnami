import { describe, expect, it } from 'bun:test';
import RootHtml from '../../src/ssr/default.html';

// The element tree RootHtml builds, as far as the test reads it.
type Element = { type: string; props: { lang?: string; id?: string; children: unknown } };

describe('RootHtml', () => {
  it('returns the expected document structure', () => {
    const element = RootHtml({ children: 'Hello' }) as unknown as Element;

    expect(element.type).toBe('html');
    expect(element.props.lang).toBe('en');

    const [head, body] = element.props.children as [Element, Element];
    expect(head.type).toBe('head');
    expect(body.type).toBe('body');

    const rootDiv = body.props.children as Element;
    expect(rootDiv.type).toBe('div');
    expect(rootDiv.props.id).toBe('root');
    expect(rootDiv.props.children).toBe('Hello');
  });
});
