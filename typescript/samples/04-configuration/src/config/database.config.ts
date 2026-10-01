import { Config, Default, Env, Int, Sensitive } from '@putnami/runtime';

export const DatabaseConfig = Config('database.primary', {
  host: Default(String, 'localhost'),
  port: Default(Int, 5432),
  name: Default(String, 'mydb'),
  user: Default(String, 'postgres'),
  // Secrets use `Sensitive` so they are redacted in config dumps and error
  // origins, and are sourced from an env var rather than a committed literal.
  password: Sensitive(Env('DATABASE_PRIMARY_PASSWORD', Default(String, ''))),
});

export const ReplicaDatabaseConfig = Config('database.replica', {
  host: Default(String, 'localhost'),
  port: Default(Int, 5432),
  name: Default(String, 'mydb'),
  user: Default(String, 'postgres'),
  password: Sensitive(Env('DATABASE_REPLICA_PASSWORD', Default(String, ''))),
});
