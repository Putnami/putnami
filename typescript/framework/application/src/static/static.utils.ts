import { mkdirSync } from 'node:fs';
import { useConfig } from '@putnami/runtime';
import { getProjectRoot, joinPath } from '@putnami/utils';
import { PutnamiConfig } from '../application/putnami.config';

let _publicFolder: undefined | string;
export function publicFolder(): string {
  if (!_publicFolder) {
    const config = useConfig(PutnamiConfig);
    _publicFolder = config.assetsDir ?? joinPath(getProjectRoot(), '.gen', config.publicFolder);

    mkdirSync(_publicFolder, { recursive: true });
  }

  return _publicFolder;
}
