import { Config, Default, Env, Int, Sensitive } from '@putnami/runtime';

export const ApiConfig = Config('api.weather', {
  baseUrl: Default(String, ''),
  // The API key is a secret: redacted via `Sensitive` and sourced from an env
  // var rather than a committed literal.
  apiKey: Sensitive(Env('WEATHER_API_KEY', Default(String, ''))),
  timeout: Default(Int, 5000),
  retries: Default(Int, 3),
});
