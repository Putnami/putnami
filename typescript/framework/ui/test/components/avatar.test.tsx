import { describe, expect, test } from 'bun:test';
import { Avatar, AvatarGroup } from '../../src/components/avatar';
import { render } from './render';

describe('Avatar', () => {
  test('renders image source when provided', () => {
    const html = render(<Avatar src='test.jpg' name='John Doe' />);
    expect(html).toContain('src="test.jpg"');
    expect(html).toContain('alt="John Doe"');
  });

  test('falls back to initials without src', () => {
    const html = render(<Avatar name='John Doe' />);
    expect(html).toContain('JD');
    expect(html).toContain('aria-label="John Doe"');
  });

  test('forwards arbitrary DOM props to the root element', () => {
    const html = render(<Avatar name='John Doe' id='profile-avatar' data-testid='avatar' />);
    expect(html).toContain('id="profile-avatar"');
    expect(html).toContain('data-testid="avatar"');
  });
});

describe('AvatarGroup', () => {
  test('renders max limit with excess indicator', () => {
    const html = render(
      <AvatarGroup max={2}>
        <Avatar name='A' />
        <Avatar name='B' />
        <Avatar name='C' />
      </AvatarGroup>,
    );
    expect(html).toContain('+1');
  });

  test('renders all children when max is not set', () => {
    const html = render(
      <AvatarGroup>
        <Avatar name='Alice' />
        <Avatar name='Bob' />
        <Avatar name='Carol' />
      </AvatarGroup>,
    );
    expect(html).toContain('Alice');
    expect(html).toContain('Bob');
    expect(html).toContain('Carol');
  });

  test('does not mutate the children prop array', () => {
    const children = [<Avatar key='a' name='A' />, <Avatar key='b' name='B' />, <Avatar key='c' name='C' />];
    const originalOrder = children.map((c) => c.props.name);
    render(<AvatarGroup>{children}</AvatarGroup>);
    const afterOrder = children.map((c) => c.props.name);
    expect(afterOrder).toEqual(originalOrder);
  });

  test('renders avatars in correct visual order (last child first in DOM due to flex-direction: row-reverse)', () => {
    const html = render(
      <AvatarGroup max={2}>
        <Avatar name='Alice' />
        <Avatar name='Bob' />
      </AvatarGroup>,
    );
    const alicePos = html.indexOf('Alice');
    const bobPos = html.indexOf('Bob');
    // After reversal, Bob appears before Alice in the DOM (row-reverse stacks them visually left-to-right as Alice, Bob)
    expect(bobPos).toBeLessThan(alicePos);
  });

  test('forwards arbitrary DOM props to the root element', () => {
    const html = render(
      <AvatarGroup id='team' data-testid='avatar-group'>
        <Avatar name='A B' />
      </AvatarGroup>,
    );
    expect(html).toContain('id="team"');
    expect(html).toContain('data-testid="avatar-group"');
  });
});
