import { describe, expect, test } from 'bun:test';
import {
  Popover,
  PopoverBody,
  PopoverCloseButton,
  type PopoverCloseButtonProps,
  PopoverHeader,
} from '../../src/components/popover';
import { clickButtonElement, renderForwardRefRoot } from './forward-ref';
import { render } from './render';

describe('Popover', () => {
  test('renders trigger only when closed', () => {
    const html = render(
      <Popover isOpen={false} content={<PopoverBody>Details</PopoverBody>}>
        <button type='button'>Trigger</button>
      </Popover>,
    );
    expect(html).toContain('button');
    expect(html).toContain('Trigger');
    expect(html).not.toContain('Details');
  });

  test('renders portal content when controlled isOpen is true', () => {
    const html = render(
      <Popover
        isOpen={true}
        content={
          <>
            <PopoverHeader>Title</PopoverHeader>
            <PopoverBody>Details</PopoverBody>
          </>
        }
      >
        <button type='button'>Trigger</button>
      </Popover>,
    );

    expect(html).toContain('Trigger');
    expect(html).toContain('Title');
    expect(html).toContain('Details');
  });

  test('sub-components forward arbitrary DOM props to their root element', () => {
    const html = render(
      <Popover
        isOpen={true}
        content={
          <>
            <PopoverHeader id='pop-head'>Title</PopoverHeader>
            <PopoverBody data-testid='pop-body'>Details</PopoverBody>
            <PopoverCloseButton onClose={() => {}} data-testid='pop-close' />
          </>
        }
      >
        <button type='button'>Trigger</button>
      </Popover>,
    );

    expect(html).toContain('id="pop-head"');
    expect(html).toContain('data-testid="pop-body"');
    expect(html).toContain('data-testid="pop-close"');
  });

  test('close button composes forwarded onClick with onClose', () => {
    let closeCalls = 0;
    let clickCalls = 0;
    const button = renderForwardRefRoot<PopoverCloseButtonProps>(PopoverCloseButton, {
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
    const button = renderForwardRefRoot<PopoverCloseButtonProps>(PopoverCloseButton, {
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
