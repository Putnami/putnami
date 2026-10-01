import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  Modal,
  ModalBody,
  ModalCloseButton,
  type ModalCloseButtonProps,
  ModalFooter,
  ModalHeader,
} from '../../src/components/modal';
import { clickButtonElement, renderForwardRefRoot } from './forward-ref';
import { render } from './render';

describe('Modal', () => {
  specTest(
    'renders nothing when closed',
    {
      feature: 'typescript/ui-system',
      requirement: 'interactive-semantics',
      check: 'a-closed-overlay-renders-nothing',
    },
    () => {
      const html = render(
        <Modal isOpen={false} onClose={() => {}}>
          <ModalBody>Content</ModalBody>
        </Modal>,
      );
      expect(html).toBe('');
    },
  );

  specTest(
    'renders portal content and roles when open',
    {
      feature: 'typescript/ui-system',
      requirement: 'interactive-semantics',
      check: 'an-overlay-exposes-dialog-semantics-while-open',
    },
    () => {
      const html = render(
        <Modal isOpen={true} onClose={() => {}}>
          <ModalHeader>Header</ModalHeader>
          <ModalBody>Content</ModalBody>
          <ModalFooter>Footer</ModalFooter>
          <ModalCloseButton onClose={() => {}} />
        </Modal>,
      );

      expect(html).toContain('role="dialog"');
      expect(html).toContain('aria-modal="true"');
      expect(html).toContain('Content');
      expect(html).toContain('aria-label="Close modal"');
    },
  );

  test('sub-components forward arbitrary DOM props to their root element', () => {
    const html = render(
      <Modal isOpen={true} onClose={() => {}}>
        <ModalHeader id='modal-head' data-testid='head'>
          Header
        </ModalHeader>
        <ModalBody id='modal-body'>Content</ModalBody>
        <ModalFooter data-section='footer'>Footer</ModalFooter>
        <ModalCloseButton onClose={() => {}} data-testid='close' />
      </Modal>,
    );

    expect(html).toContain('id="modal-head"');
    expect(html).toContain('data-testid="head"');
    expect(html).toContain('id="modal-body"');
    expect(html).toContain('data-section="footer"');
    expect(html).toContain('data-testid="close"');
  });

  test('close button composes forwarded onClick with onClose', () => {
    let closeCalls = 0;
    let clickCalls = 0;
    const button = renderForwardRefRoot<ModalCloseButtonProps>(ModalCloseButton, {
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
    const button = renderForwardRefRoot<ModalCloseButtonProps>(ModalCloseButton, {
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
