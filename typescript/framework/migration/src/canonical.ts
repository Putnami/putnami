/**
 * Canonical state-store identity for a migration. Mirrors the Go
 * `protocolmigration.CanonicalID` function — both runners write the
 * same `${datasource}:${name}` shape so they can share a database.
 */
export const DEFAULT_DATASOURCE = 'default';

export function canonicalMigrationId(datasource: string | undefined, name: string): string {
  const ds = datasource && datasource !== '' ? datasource : DEFAULT_DATASOURCE;
  return `${ds}:${name}`;
}
