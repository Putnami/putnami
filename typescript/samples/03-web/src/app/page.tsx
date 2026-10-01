import { page } from '@putnami/web';
import Counter from './Counter.island';

function HomePage() {
  return (
    <div>
      <h2>Welcome</h2>
      <p>This sample demonstrates React SSR with Putnami.</p>
      <ul>
        <li>File-based routing with pages and layouts</li>
        <li>Server-side data fetching with loaders</li>
        <li>Form submissions with actions</li>
        <li>Dynamic route segments</li>
        <li>Static rendering (SSG) — this page ships zero base JS</li>
        <li>Islands — only the counter below hydrates</li>
      </ul>
      <p>
        <Counter start={0} />
      </p>
      <p>
        <a href='/tasks'>View Tasks</a>
      </p>
    </div>
  );
}

// Rendered statically at build time (SSG). Only the island hydrates on the client.
export default page().static().render(HomePage);
