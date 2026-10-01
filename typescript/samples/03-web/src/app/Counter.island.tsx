import { useState } from 'react';
import { island } from '@putnami/web';

function Counter({ start = 0 }: { start?: number }) {
  const [count, setCount] = useState(start);
  return (
    // Declarative click tracking: no handler, no import. One capturing listener
    // on the document reads `data-track` as the action name and every
    // `data-track-*` attribute as a property (`data-track-value` -> `value`).
    <button
      type='button'
      data-track='counter_click'
      data-track-value='1'
      onClick={() => setCount((value) => value + 1)}
    >
      Clicked {count} times
    </button>
  );
}

// A hydration island: ships and hydrates its own JS when scrolled into view.
// The rest of the page stays static HTML.
export default island().load('visible').render(Counter);
