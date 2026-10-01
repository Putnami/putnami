import { fileExists, makeDir, readFileContent, writeFileContent } from './fs.utils';
import { getDirectoryName, joinPosixPath, toPosixPath } from './path.utils';

export class GeneratorHelper {
  private heads = new Set<string>();

  private buffer = [] as string[];

  constructor(private fileName: string) {}

  /**
   * Returns the import specifier for a file, in forward-slash form: a Windows
   * separator in a specifier is an escape sequence, and in a derived module
   * name it is not a valid identifier character.
   */
  normalizeImport(file: string) {
    let moduleSource = toPosixPath(file).replace(/\.(ts|tsx|js|jsx)$/, '');
    if (moduleSource.charAt(0) === '/') {
      moduleSource = `.${moduleSource}`;
    } else if (moduleSource.charAt(0) !== '.') {
      moduleSource = `./${moduleSource}`;
    }

    return moduleSource;
  }

  getModuleName(file: string, suffix = ''): string {
    const moduleSource = this.normalizeImport(file);
    return moduleSource.replace(/[.\-/[\]()]/g, '_') + suffix;
  }

  addImportDefault(file: string, suffix = '', relativeTo = ''): string {
    const moduleSource = this.normalizeImport(file);
    const moduleName = moduleSource.replace(/[.\-/[\]()]/g, '_') + suffix;
    this.heads.add(`import ${moduleName} from '${joinPosixPath(relativeTo, moduleSource)}';`);

    return moduleName;
  }

  addImportModule(file: string, relativeTo = ''): string {
    const moduleSource = this.normalizeImport(file);
    const moduleName = moduleSource.replace(/[.\-/[\]()]/g, '_');
    this.heads.add(`import * as ${moduleName} from '${joinPosixPath(relativeTo, moduleSource)}';`);

    return moduleName;
  }

  getLazyImport(file: string, relativeTo = ''): string {
    const moduleSource = this.normalizeImport(file);
    return `() => import('${joinPosixPath(relativeTo, moduleSource)}')`;
  }

  appendHead(line: string) {
    this.heads.add(line);

    return this;
  }

  append(line: string) {
    this.buffer.push(line);

    return this;
  }

  write() {
    const file = this.fileName;
    const heads = [...this.heads];
    heads.push('');

    makeDir(getDirectoryName(file), { recursive: true });
    const data = heads.concat(this.buffer).join('\n');
    if (!fileExists(file) || readFileContent(file, 'utf8') !== data) {
      writeFileContent(file, data);
    }

    return this.fileName;
  }
}
