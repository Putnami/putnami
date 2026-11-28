// Run in its own process, without test/setup.ts: that preload defines a global
// `document`, which makes Emotion insert into a browser sheet instead of
// recording the CSS a server render extracts.
import { Suspense, use } from 'react';
import { renderToReadableStream } from 'react-dom/server';
// The provider `@putnami/web` exports through `useDocumentMeta`, by its source path: the package
// entry does not export it, and both paths resolve to the same module.
import { DocumentMetaProvider } from '../../../web/src/client/document/document-context';
import { styled } from '../../src/emotion';
import { ThemeProvider } from '../../src/theme/provider';

const documentMeta: { styleExtractors?: Array<() => unknown[]> } = {};
const ShellBox = styled.div`color: rgb(1, 2, 3);`;
const LateBox = styled.div`color: rgb(4, 5, 6);`;
let open!: () => void;
const data = new Promise<void>((resolve) => {
  open = resolve;
});
function Late() {
  use(data);
  return <LateBox>late</LateBox>;
}

const stream = await renderToReadableStream(
  <DocumentMetaProvider meta={documentMeta as never}>
    <ThemeProvider colorMode='light'>
      <ShellBox>shell</ShellBox>
      <Suspense fallback={null}>
        <Late />
      </Suspense>
    </ThemeProvider>
  </DocumentMetaProvider>,
);
const extractor = documentMeta.styleExtractors?.[0];
const calls: unknown[] = [];
calls.push(extractor?.(), extractor?.());
open();
await stream.allReady;
calls.push(extractor?.(), extractor?.());
process.stdout.write(JSON.stringify(calls));
