import { describe, expect, test } from 'bun:test';
import { Badge } from '../../src/components/badge';
import { render } from './render';

describe('Badge', () => {
  test('renders children correctly', () => {
    const html = render(<Badge>New Feature</Badge>);
    expect(html).toContain('New Feature');
  });

  test('applies variant and color parameters without crashing', () => {
    // We are just verifying it renders without throwing and applies some styling classes.
    const htmlSolid = render(
      <Badge variant='solid' colorScheme='error'>
        Error
      </Badge>,
    );
    expect(htmlSolid).toContain('Error');

    const htmlOutline = render(
      <Badge variant='outline' size='lg'>
        Outline Large
      </Badge>,
    );
    expect(htmlOutline).toContain('Outline Large');
  });

  test('forwards arbitrary DOM props (id, data-*, title) to the root element', () => {
    const html = render(
      <Badge id='status-badge' data-testid='badge' title='Status'>
        Live
      </Badge>,
    );
    expect(html).toContain('id="status-badge"');
    expect(html).toContain('data-testid="badge"');
    expect(html).toContain('title="Status"');
  });
});
