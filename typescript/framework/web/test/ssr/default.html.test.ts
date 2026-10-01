import { describe, expect, it } from 'bun:test';
import RootHtml from '../../src/ssr/default.html';

describe('RootHtml', () => {
  it('returns the expected document structure', () => {
    const element = RootHtml({ children: 'Hello' }) as never;

    expect(element.type).toBe('html');
    expect(element.props.lang).toBe('en');

    const [head, body] = element.props.children;
    expect(head.type).toBe('head');
    expect(body.type).toBe('body');

    const rootDiv = body.props.children;
    expect(rootDiv.type).toBe('div');
    expect(rootDiv.props.id).toBe('root');
    expect(rootDiv.props.children).toBe('Hello');
  });
});
