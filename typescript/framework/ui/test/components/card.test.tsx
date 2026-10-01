import { describe, expect, test } from 'bun:test';
import { Card, CardBody, CardFooter, CardHeader } from '../../src/components/card';
import { render } from './render';

describe('Card', () => {
  test('renders card subcomponents', () => {
    const html = render(
      <Card variant='outline'>
        <CardHeader>Header Content</CardHeader>
        <CardBody>Body Content</CardBody>
        <CardFooter>Footer Content</CardFooter>
      </Card>,
    );

    expect(html).toContain('Header Content');
    expect(html).toContain('Body Content');
    expect(html).toContain('Footer Content');
  });
});
