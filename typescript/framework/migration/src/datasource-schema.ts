/**
 * One source's statement that its migrations on `datasource` run in `schema`.
 * An empty `schema` states nothing: the source follows whatever `search_path`
 * the connection already has.
 */
export interface DatasourceSchemaClaim {
  datasource: string;
  schema?: string;
  namespace: string;
}

/**
 * Two sources put one datasource in two schemas. The runner applies a
 * datasource's schema as the `search_path` of every migration on it, so the
 * path would be ambiguous and the shared state store could not tell the two
 * apart.
 */
export class DatasourceSchemaConflictError extends Error {
  constructor(
    readonly datasource: string,
    readonly schemas: readonly [string, string],
    readonly namespaces: readonly [string, string],
  ) {
    super(
      // JSON quoting matches Go's %q for printable names, so both languages
      // print one message for any schema a runner accepts.
      `datasource ${q(datasource)} has conflicting schemas across sources: ${q(schemas[0])} and ${q(schemas[1])} (namespaces ${q(namespaces[0])} and ${q(namespaces[1])})`,
    );
    this.name = 'DatasourceSchemaConflictError';
  }
}

/** A double-quoted, escaped name. */
function q(name: string): string {
  return JSON.stringify(name);
}

/**
 * Resolves each datasource's owning schema from the claims of its sources.
 *
 * The one rule both the runner (at apply time) and the bundle generator (at
 * build time) enforce, so a workload that cannot migrate fails `putnami build`
 * with the message it would otherwise meet at apply. Mirrors Go's
 * `migration.ResolveDatasourceSchemas`.
 *
 * @param claims - Every source's datasource, declared schema and namespace.
 * @returns The schema of each datasource that has one.
 * @throws DatasourceSchemaConflictError when two claims name different schemas
 *   for the same datasource.
 */
export function resolveDatasourceSchemas(claims: Iterable<DatasourceSchemaClaim>): Map<string, string> {
  const owners = new Map<string, { schema: string; namespace: string }>();
  for (const claim of claims) {
    const schema = claim.schema;
    if (!schema) continue;
    const first = owners.get(claim.datasource);
    if (first === undefined) {
      owners.set(claim.datasource, { schema, namespace: claim.namespace });
    } else if (first.schema !== schema) {
      throw new DatasourceSchemaConflictError(
        claim.datasource,
        [first.schema, schema],
        [first.namespace, claim.namespace],
      );
    }
  }
  return new Map([...owners].map(([datasource, { schema }]) => [datasource, schema]));
}
