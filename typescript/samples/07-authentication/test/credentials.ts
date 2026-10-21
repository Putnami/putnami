import { resetConfigLoader } from '@putnami/runtime';

type CredentialName = 'OAUTH_CLIENT_ID' | 'OAUTH_CLIENT_SECRET';

const CREDENTIAL_NAMES: readonly CredentialName[] = ['OAUTH_CLIENT_ID', 'OAUTH_CLIENT_SECRET'];

/**
 * Sets the OAuth credential variables `conf/.env.yaml` reads, unsetting each one
 * the argument omits, and reloads the configuration so it interpolates them.
 * `configData` sets `CONFIG_DATA`, the inline YAML source that overrides the
 * committed files, and is unset when omitted. Returns a function that restores
 * the previous values and reloads again.
 */
export function setOAuthCredentials(
  values: Partial<Record<CredentialName, string>>,
  options: { configData?: string } = {},
): () => void {
  const previous = [...CREDENTIAL_NAMES, 'CONFIG_DATA'].map((name) => [name, process.env[name]] as const);
  for (const name of CREDENTIAL_NAMES) {
    setVariable(name, values[name]);
  }
  setVariable('CONFIG_DATA', options.configData);
  resetConfigLoader();
  return () => {
    for (const [name, value] of previous) {
      setVariable(name, value);
    }
    resetConfigLoader();
  };
}

function setVariable(name: string, value: string | undefined): void {
  if (value === undefined) {
    delete process.env[name];
  } else {
    process.env[name] = value;
  }
}
