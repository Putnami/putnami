import { describe, expect, test } from 'bun:test';
import {
  Drawer,
  DrawerBody,
  DrawerCloseButton,
  type DrawerCloseButtonProps,
  DrawerFooter,
  DrawerHeader,
} from '../../src/components/drawer';
import { clickButtonElement, renderForwardRefRoot } from './forward-ref';
import { render } from './render';

describe('Drawer', () => {
  test('renders nothing when closed', () => {
    const html = render(
      <Drawer isOpen={false} onClose={() => {}}>
        <DrawerBody>Content</DrawerBody>
      </Drawer>,
    );
    expect(html).toBe('');
  });

  test('renders portal content and roles when open', () => {
    const html = render(
      <Drawer isOpen={true} onClose={() => {}}>
        <DrawerHeader>Header</DrawerHeader>
        <DrawerBody>Content</DrawerBody>
        <DrawerFooter>Footer</DrawerFooter>
        <DrawerCloseButton onClose={() => {}} />
      </Drawer>,
    );
    // Rendered via createPortal, react-dom/server renderToStaticMarkup will output portal children
    expect(html).toContain('role="dialog"');
    expect(html).toContain('aria-modal="true"');
    expect(html).toContain('Content');
    expect(html).toContain('aria-label="Close drawer"');
  });

  test('sub-components forward arbitrary DOM props to their root element', () => {
    const html = render(
      <Drawer isOpen={true} onClose={() => {}}>
        <DrawerHeader id='drawer-head' data-testid='head'>
          Header
        </DrawerHeader>
        <DrawerBody id='drawer-body'>Content</DrawerBody>
        <DrawerFooter data-section='footer'>Footer</DrawerFooter>
        <DrawerCloseButton onClose={() => {}} data-testid='close' />
      </Drawer>,
    );

    expect(html).toContain('id="drawer-head"');
    expect(html).toContain('data-testid="head"');
    expect(html).toContain('id="drawer-body"');
    expect(html).toContain('data-section="footer"');
    expect(html).toContain('data-testid="close"');
  });

  test('close button composes forwarded onClick with onClose', () => {
    let closeCalls = 0;
    let clickCalls = 0;
    const button = renderForwardRefRoot<DrawerCloseButtonProps>(DrawerCloseButton, {
      onClose: () => {
        closeCalls += 1;
      },
      onClick: () => {
        clickCalls += 1;
      },
    });

    clickButtonElement(button);

    expect(clickCalls).toBe(1);
    expect(closeCalls).toBe(1);
  });

  test('close button skips onClose when forwarded onClick prevents default', () => {
    let closeCalls = 0;
    const button = renderForwardRefRoot<DrawerCloseButtonProps>(DrawerCloseButton, {
      onClose: () => {
        closeCalls += 1;
      },
      onClick: (event) => {
        event.preventDefault();
      },
    });

    clickButtonElement(button);

    expect(closeCalls).toBe(0);
  });
});
