export default function HomePage() {
  return (
    <div>
      <h2>Welcome to Project Tracker</h2>
      <p>A complete production-shaped application built with Putnami.</p>
      <h3>Features</h3>
      <ul>
        <li>Independent projects/tasks modules with package-style boundaries</li>
        <li>PostgreSQL with Repository pattern and type-safe tables</li>
        <li>Typed event contracts and cross-module handlers</li>
        <li>Live activity streams for each module</li>
        <li>React SSR with layouts, loaders, actions, and forms</li>
        <li>REST API with endpoint builder and validation</li>
        <li>Cross-module feature: project kickoff task + auto-completion status</li>
      </ul>
      <p>
        <a href='/projects'>View Projects</a>
      </p>
      <p>
        <a href='/tasks'>Open Tasks Board</a>
      </p>
    </div>
  );
}
