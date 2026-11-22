import { join } from 'node:path';
import ts from 'typescript';
import { type Block, NO_CHECK } from './markdown';

/** One TypeScript block of a markdown document. */
export interface Example {
  /** The document's path, as reported in findings. */
  doc: string;
  block: Block;
}

/** An error an example has, at its markdown line. */
export interface Finding {
  doc: string;
  line: number;
  message: string;
}

/** The fence languages the check type-checks. */
export const LANGUAGES = new Set(['ts', 'typescript', 'tsx']);

/**
 * The diagnostics a documentation fragment may have: it may use a name it does
 * not declare, leave a name unused, untyped or possibly undefined, return
 * outside a function, list signatures without bodies, or declare a name again
 * to show an alternative. Every other diagnostic fails the example.
 */
const TOLERATED = new Set([
  2304, // Cannot find name.
  2552, // Cannot find name; did you mean.
  2580, // Cannot find name 'require' and the like.
  2582, // Cannot find name 'describe' and the like.
  2591, // Cannot find name 'process' and the like.
  2593, // Cannot find name 'it' and the like.
  2584, // Cannot find name 'document' and the like: a browser global.
  18_004, // No value exists in scope for a shorthand property.
  1108, // A 'return' statement can only be used within a function body.
  1375, // 'await' at the top level of a script.
  7016, // No declaration file for a module.
  7028, // Unused label.
  6133, // Declared but never read.
  6138, // Property declared but never read.
  6192, // All imports are unused.
  6196, // Declared but never used.
  6198, // All destructured elements are unused.
  6205, // All type parameters are unused.
  7005, // Implicitly has an 'any' type.
  7006, // Parameter implicitly has an 'any' type.
  7008, // Member implicitly has an 'any' type.
  7019, // Rest parameter implicitly has an 'any[]' type.
  7031, // Binding element implicitly has an 'any' type.
  7034, // Variable implicitly has type 'any' in some locations.
  18_046, // Is of type 'unknown'.
  18_047, // Is possibly 'null'.
  18_048, // Is possibly 'undefined'.
  2531, // Object is possibly 'null'.
  2532, // Object is possibly 'undefined'.
  2533, // Object is possibly 'null' or 'undefined'.
  2454, // Used before being assigned.
  2564, // Property has no initializer.
  2390, // Constructor implementation is missing: a signature listing.
  2391, // Function implementation is missing: a signature listing.
  2300, // Duplicate identifier: a block shows alternatives.
  2323, // Cannot redeclare exported variable.
  2393, // Duplicate function implementation.
  2451, // Cannot redeclare block-scoped variable.
  2528, // A module cannot have multiple default exports.
  2571, // Object is of type 'unknown'.
  2347, // Untyped function calls may not accept type arguments: the callee is a name the block does not declare.
]);

/** Cannot find module: tolerated outside the workspace framework. */
const CANNOT_FIND_MODULE = new Set([2307, 2792, 2882]);

interface Source {
  example: Example;
  path: string;
  text: string;
}

/**
 * Type-checks each example as a module of a project in `dir`, whose
 * tsconfig.json and node_modules resolve the framework packages, and returns
 * the findings, sorted by document and line.
 */
export function check(examples: Example[], dir: string): Finding[] {
  const options = compilerOptions(dir);
  const sources = examples.map((example, index): Source => {
    const extension = example.block.lang === 'tsx' ? 'tsx' : 'ts';
    return {
      example,
      path: join(dir, '.examples', `${index}.${extension}`),
      text: moduleText(example.block),
    };
  });

  // A fragment uses a name another block of its document imports: import it
  // the same way, on the fragment's first line. A global of the same name,
  // such as the DOM's Plugin, does not hide the import the document meant.
  const tables = importTables(examples);
  // A name a block of the document declares itself, such as a `logger`
  // variable next to an imported `logger` plugin, is not carried.
  for (const { doc, block } of examples) {
    const table = tables.get(doc);
    if (!table) continue;
    for (const name of names(block.code, block.lang).declared) table.delete(name);
  }
  for (const source of sources) {
    const table = tables.get(source.example.doc);
    if (!table || table.size === 0) continue;
    const { used, declared, imported } = names(source.text, source.example.block.lang);
    const carried = [...used]
      .filter((name) => !declared.has(name) && !imported.has(name) && table.has(name))
      .map((name) => table.get(name));
    if (carried.length > 0) source.text = `${[...new Set(carried)].join(' ')} ${source.text}`;
  }
  let program = createProgram([...sources, exportsSource(dir)], options);

  // A name no block of the document imports may still be one of the
  // framework's exports, such as `endpoint` in a fragment that shows only a
  // handler: import it when exactly one package exports it and every property
  // the fragment reads from it exists, so `logger.info` on a logger variable
  // is not taken for the `logger` plugin.
  const index = exportIndex(program, dir);
  let imported = false;
  for (const source of sources) {
    const file = program.getSourceFile(source.path);
    if (!file) continue;
    const statements = new Set<string>();
    for (const diagnostic of program.getSemanticDiagnostics(file)) {
      if (diagnostic.code !== 2304 && diagnostic.code !== 2552) continue;
      const name = /'([^']+)'/.exec(ts.flattenDiagnosticMessageText(diagnostic.messageText, ' '))?.[1];
      const candidate = name ? index.get(name) : undefined;
      if (name && candidate && readsOnly(file, name, candidate, program.getTypeChecker())) {
        statements.add(`import { ${name} } from '${candidate.module}';`);
      }
    }
    if (statements.size > 0) {
      source.text = `${[...statements].join(' ')} ${source.text}`;
      imported = true;
    }
  }
  if (imported) program = createProgram(sources, options);

  const findings: Finding[] = [];
  for (const source of sources) {
    const file = program.getSourceFile(source.path);
    if (!file) continue;
    const { doc, block } = source.example;
    const at = (diagnostic: ts.Diagnostic) =>
      block.line + 1 + file.getLineAndCharacterOfPosition(diagnostic.start ?? 0).line;
    const syntax = program.getSyntacticDiagnostics(file);
    const first = syntax[0];
    if (first) {
      findings.push({
        doc,
        line: at(first),
        message: `the block does not parse: ${ts.flattenDiagnosticMessageText(first.messageText, ' ')}; complete it, or mark the fence \`${NO_CHECK}\` when it is not meant to compile`,
      });
      continue;
    }
    for (const diagnostic of program.getSemanticDiagnostics(file)) {
      if (!reported(diagnostic)) continue;
      findings.push({
        doc,
        line: at(diagnostic),
        message: ts.flattenDiagnosticMessageText(diagnostic.messageText, ' '),
      });
    }
  }
  return findings.sort((a, b) => (a.doc === b.doc ? a.line - b.line : a.doc < b.doc ? -1 : 1));
}

/**
 * Turns a block into the text of a module. A block that does not parse as a
 * module but parses as the members of a class, such as a method of a
 * repository, is checked inside a class whose other members are untyped. The
 * wrapper opens on the block's first line, so lines keep their numbers.
 */
function moduleText(block: Block): string {
  const text = `${block.code}\nexport {};\n`;
  if (parses(text, block.lang)) return text;
  const members = `class __Example { [member: string]: any; ${block.code}\n}\nexport {};\n`;
  return parses(members, block.lang) ? members : text;
}

function parses(text: string, lang: string): boolean {
  const kind = lang === 'tsx' ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const file = ts.createSourceFile('block.ts', text, ts.ScriptTarget.Latest, false, kind);
  // parseDiagnostics is not part of the public typings, but every source file
  // carries it.
  return (file as unknown as { parseDiagnostics: readonly ts.Diagnostic[] }).parseDiagnostics.length === 0;
}

/**
 * A property read on a value that may be undefined or is unknown: the same
 * null-safety gap as 18048 and 18046, reported as a missing property.
 */
const UNSAFE_RECEIVER = /^Property '[^']+' does not exist on type '(unknown|.* \| undefined)'\.$/;

/** The virtual module that imports every framework package the project depends on. */
const EXPORTS_FILE = '__exports.ts';

function frameworkPackages(dir: string): string[] {
  const manifest = JSON.parse(ts.sys.readFile(join(dir, 'package.json')) ?? '{}') as {
    dependencies?: Record<string, string>;
  };
  return Object.keys(manifest.dependencies ?? {})
    .filter((name) => name.startsWith('@putnami/'))
    .sort((a, b) => (a === '@putnami/application' ? -1 : b === '@putnami/application' ? 1 : a.localeCompare(b)));
}

function exportsSource(dir: string): Source {
  const text = frameworkPackages(dir)
    .map((name, i) => `import * as m${i} from '${name}';`)
    .join('\n');
  return {
    example: { doc: EXPORTS_FILE, block: { lang: 'ts', attributes: [], line: 0, code: '' } },
    path: join(dir, '.examples', EXPORTS_FILE),
    text: `${text}\nexport {};\n`,
  };
}

interface Export {
  module: string;
  symbol: ts.Symbol;
}

/**
 * Maps each name the framework packages export to the one package that
 * exports it; a name two packages export as different symbols is left out.
 */
function exportIndex(program: ts.Program, dir: string): Map<string, Export> {
  const checker = program.getTypeChecker();
  const file = program.getSourceFile(join(dir, '.examples', EXPORTS_FILE));
  const index = new Map<string, Export>();
  const ambiguous = new Set<string>();
  if (!file) return index;
  for (const statement of file.statements) {
    if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier)) continue;
    const moduleSymbol = checker.getSymbolAtLocation(statement.moduleSpecifier);
    if (!moduleSymbol) continue;
    for (const exported of checker.getExportsOfModule(moduleSymbol)) {
      const name = exported.getName();
      const symbol = exported.flags & ts.SymbolFlags.Alias ? checker.getAliasedSymbol(exported) : exported;
      const known = index.get(name);
      if (ambiguous.has(name) || (known && known.symbol === symbol)) continue;
      if (known) {
        index.delete(name);
        ambiguous.add(name);
        continue;
      }
      index.set(name, { module: statement.moduleSpecifier.text, symbol });
    }
  }
  return index;
}

/**
 * Whether every use of `name` in the file fits the export: it is called,
 * constructed, rendered as a JSX tag or named as a type, and each property the
 * file reads from it exists. A bare value use, as `handler` in
 * `get('/', handler)`, is a variable the fragment does not declare.
 */
function readsOnly(file: ts.SourceFile, name: string, candidate: Export, checker: ts.TypeChecker): boolean {
  const flags = candidate.symbol.flags;
  const isValue = (flags & ts.SymbolFlags.Value) !== 0;
  const isType = (flags & ts.SymbolFlags.Type) !== 0;
  const type = isValue ? checker.getTypeOfSymbol(candidate.symbol) : undefined;
  let fits = true;
  // A name used only as a type, such as `Server`, may be a runtime's type
  // the fragment means; it takes an import only with a value use.
  let valueUse = false;
  const visit = (node: ts.Node) => {
    if (!fits) return;
    if (ts.isIdentifier(node) && node.text === name) {
      const parent = node.parent;
      if (ts.isPropertyAccessExpression(parent) && parent.name === node) return;
      valueUse ||= !(ts.isTypeReferenceNode(parent) || ts.isExpressionWithTypeArguments(parent));
      if (ts.isTypeReferenceNode(parent) || ts.isExpressionWithTypeArguments(parent)) {
        fits = isType || ts.isExpressionWithTypeArguments(parent);
      } else if (ts.isCallExpression(parent) && ts.isDecorator(parent.parent)) {
        fits = false;
      } else if (
        (ts.isCallExpression(parent) && parent.expression === node) ||
        (ts.isNewExpression(parent) && parent.expression === node) ||
        ((ts.isJsxOpeningElement(parent) || ts.isJsxSelfClosingElement(parent) || ts.isJsxClosingElement(parent)) &&
          parent.tagName === node)
      ) {
        fits = isValue;
      } else if (ts.isPropertyAccessExpression(parent) && parent.expression === node) {
        fits = type !== undefined && checker.getPropertyOfType(type, parent.name.text) !== undefined;
      } else {
        fits = false;
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return fits && valueUse;
}

function reported(diagnostic: ts.Diagnostic): boolean {
  if (TOLERATED.has(diagnostic.code)) return false;
  const message = ts.flattenDiagnosticMessageText(diagnostic.messageText, ' ');
  if (diagnostic.code === 2339 && UNSAFE_RECEIVER.test(message)) return false;
  // A caught value is unknown under strict mode, like 18046.
  if (diagnostic.code === 2345 && message.startsWith("Argument of type 'unknown' is not assignable")) return false;
  if (CANNOT_FIND_MODULE.has(diagnostic.code)) {
    const specifier = /'([^']+)'/.exec(message)?.[1] ?? '';
    return specifier.startsWith('@putnami/');
  }
  return true;
}

function compilerOptions(dir: string): ts.CompilerOptions {
  const configFile = ts.findConfigFile(dir, ts.sys.fileExists);
  if (!configFile) throw new Error(`no tsconfig.json above ${dir}`);
  const { config, error } = ts.readConfigFile(configFile, ts.sys.readFile);
  if (error) throw new Error(ts.flattenDiagnosticMessageText(error.messageText, ' '));
  const parsed = ts.parseJsonConfigFileContent(config, ts.sys, dir);
  return {
    ...parsed.options,
    noEmit: true,
    declaration: false,
    sourceMap: false,
    rootDir: undefined,
    noUnusedLocals: false,
    noUnusedParameters: false,
    noPropertyAccessFromIndexSignature: false,
    noImplicitOverride: false,
  };
}

function createProgram(sources: Source[], options: ts.CompilerOptions): ts.Program {
  const virtual = new Map(sources.map((source) => [source.path, source.text]));
  const host = ts.createCompilerHost(options);
  const getSourceFile = host.getSourceFile.bind(host);
  host.getSourceFile = (fileName, languageVersion, onError, shouldCreate) => {
    const text = virtual.get(fileName);
    if (text !== undefined) return ts.createSourceFile(fileName, text, languageVersion, true);
    return getSourceFile(fileName, languageVersion, onError, shouldCreate);
  };
  const fileExists = host.fileExists.bind(host);
  host.fileExists = (fileName) => virtual.has(fileName) || fileExists(fileName);
  const readFile = host.readFile.bind(host);
  host.readFile = (fileName) => virtual.get(fileName) ?? readFile(fileName);
  return ts.createProgram({ rootNames: [...virtual.keys()], options, host });
}

/**
 * The identifiers a block uses, the names its top-level statements declare,
 * and the names it imports.
 */
function names(text: string, lang: string): { used: Set<string>; declared: Set<string>; imported: Set<string> } {
  const kind = lang === 'tsx' ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const file = ts.createSourceFile('block.ts', text, ts.ScriptTarget.Latest, true, kind);
  const used = new Set<string>();
  const declared = new Set<string>();
  const imported = new Set<string>();
  const bind = (name: ts.BindingName) => {
    if (ts.isIdentifier(name)) {
      declared.add(name.text);
      return;
    }
    for (const element of name.elements) {
      if (ts.isBindingElement(element)) bind(element.name);
    }
  };
  for (const statement of file.statements) {
    if (ts.isImportDeclaration(statement)) {
      const clause = statement.importClause;
      if (clause?.name) imported.add(clause.name.text);
      const bindings = clause?.namedBindings;
      if (bindings && ts.isNamespaceImport(bindings)) imported.add(bindings.name.text);
      else if (bindings) for (const element of bindings.elements) imported.add(element.name.text);
    } else if (ts.isVariableStatement(statement)) {
      for (const declaration of statement.declarationList.declarations) bind(declaration.name);
    } else if (
      (ts.isFunctionDeclaration(statement) ||
        ts.isClassDeclaration(statement) ||
        ts.isInterfaceDeclaration(statement) ||
        ts.isTypeAliasDeclaration(statement) ||
        ts.isEnumDeclaration(statement)) &&
      statement.name
    ) {
      declared.add(statement.name.text);
    }
  }
  const visit = (node: ts.Node) => {
    if (ts.isIdentifier(node) && !(ts.isPropertyAccessExpression(node.parent) && node.parent.name === node)) {
      used.add(node.text);
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return { used, declared, imported };
}

/**
 * Maps, per document, each name a block imports to a statement that imports
 * only that name. A name two blocks import differently is left out.
 */
function importTables(examples: Example[]): Map<string, Map<string, string>> {
  const tables = new Map<string, Map<string, string>>();
  const ambiguous = new Map<string, Set<string>>();
  for (const { doc, block } of examples) {
    const table = tables.get(doc) ?? new Map<string, string>();
    tables.set(doc, table);
    const skip = ambiguous.get(doc) ?? new Set<string>();
    ambiguous.set(doc, skip);
    const kind = block.lang === 'tsx' ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
    const file = ts.createSourceFile('block', block.code, ts.ScriptTarget.Latest, false, kind);
    for (const statement of file.statements) {
      if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier)) continue;
      const clause = statement.importClause;
      if (!clause) continue;
      const from = JSON.stringify(statement.moduleSpecifier.text);
      const type = clause.isTypeOnly ? 'type ' : '';
      const add = (name: string, text: string) => {
        if (skip.has(name)) return;
        const known = table.get(name);
        if (known !== undefined && known !== text) {
          table.delete(name);
          skip.add(name);
          return;
        }
        table.set(name, text);
      };
      if (clause.name) add(clause.name.text, `import ${type}${clause.name.text} from ${from};`);
      const bindings = clause.namedBindings;
      if (bindings && ts.isNamespaceImport(bindings)) {
        add(bindings.name.text, `import ${type}* as ${bindings.name.text} from ${from};`);
      } else if (bindings) {
        for (const element of bindings.elements) {
          const imported = element.propertyName ? `${element.propertyName.getText(file)} as ` : '';
          const elementType = element.isTypeOnly && !clause.isTypeOnly ? 'type ' : '';
          add(element.name.text, `import ${type}{ ${elementType}${imported}${element.name.text} } from ${from};`);
        }
      }
    }
  }
  return tables;
}
