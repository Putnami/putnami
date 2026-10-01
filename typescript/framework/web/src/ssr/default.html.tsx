import type { ReactNode } from 'react';

export function RootHtml({ children }: { children?: ReactNode }) {
  return (
    <html lang='en'>
      <head />
      <body>
        {/* biome-ignore lint/correctness/useUniqueElementIds: root id is required */}
        <div id='root'>{children}</div>
      </body>
    </html>
  );
}

export default RootHtml;
