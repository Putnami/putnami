// Finds what a module needs from Bun outside a `typeof Bun` guard, for the
// node-target check of the build.
//
// Input, one JSON document on stdin:
//   { typescript, root, files: ["<path relative to root>"] }
// Output, one JSON document on stdout:
//   { files: [{ path, uses: [{ line, column }], imports: ["bun:sqlite"] }],
//     errors: [{ path, message }] }
//
// The script only parses: it loads the TypeScript parser from the directory
// `typescript` names and never type-checks or resolves a module.
//
// A use is an identifier `Bun` in a value position that no enclosing scope
// binds (an ambient `declare` binds nothing), or a non-optional member access
// on `globalThis.Bun`. An import is a static import or re-export of `bun` or a
// `bun:` module that is not type-only, or an `import()` or `require()` of one.
//
// A use or an import is guarded when it runs only where a `typeof Bun` test
// has shown that Bun is present: the operand of `typeof` itself, the branch of
// an `if`, a conditional expression or a logical expression that the test
// selects, and the statements after an `if` whose branch leaves the block when
// Bun is absent. A test is `typeof Bun` compared with a string literal
// (`'undefined'` means absent, any other type present), its negation, and the
// `&&` and `||` of tests.

import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import path from "node:path";

const input = JSON.parse(await Bun.stdin.text());
const ts = createRequire(import.meta.url)(input.typescript);

function scriptKind(file) {
  if (file.endsWith(".tsx")) return ts.ScriptKind.TSX;
  if (file.endsWith(".jsx")) return ts.ScriptKind.JSX;
  if (/\.(ts|mts|cts)$/.test(file)) return ts.ScriptKind.TS;
  return ts.ScriptKind.JS;
}

function unwrap(node) {
  while (
    node &&
    (ts.isParenthesizedExpression(node) ||
      ts.isAsExpression(node) ||
      ts.isNonNullExpression(node) ||
      ts.isTypeAssertionExpression(node) ||
      (ts.isSatisfiesExpression && ts.isSatisfiesExpression(node)))
  ) {
    node = node.expression;
  }
  return node;
}

const UNKNOWN = { whenTrue: false, whenFalse: false };

// knowledge returns what a condition proves about Bun: whenTrue says Bun is
// present when it holds, whenFalse that Bun is present when it does not.
function knowledge(node) {
  node = unwrap(node);
  if (!node) return UNKNOWN;
  if (ts.isPrefixUnaryExpression(node) && node.operator === ts.SyntaxKind.ExclamationToken) {
    const inner = knowledge(node.operand);
    return { whenTrue: inner.whenFalse, whenFalse: inner.whenTrue };
  }
  if (!ts.isBinaryExpression(node)) return UNKNOWN;
  const op = node.operatorToken.kind;
  if (op === ts.SyntaxKind.AmpersandAmpersandToken) {
    const left = knowledge(node.left);
    const right = knowledge(node.right);
    return { whenTrue: left.whenTrue || right.whenTrue, whenFalse: left.whenFalse && right.whenFalse };
  }
  if (op === ts.SyntaxKind.BarBarToken) {
    const left = knowledge(node.left);
    const right = knowledge(node.right);
    return { whenTrue: left.whenTrue && right.whenTrue, whenFalse: left.whenFalse || right.whenFalse };
  }
  const equal = op === ts.SyntaxKind.EqualsEqualsEqualsToken || op === ts.SyntaxKind.EqualsEqualsToken;
  const unequal = op === ts.SyntaxKind.ExclamationEqualsEqualsToken || op === ts.SyntaxKind.ExclamationEqualsToken;
  if (!equal && !unequal) return UNKNOWN;
  let typeOf = unwrap(node.left);
  let literal = unwrap(node.right);
  if (!ts.isTypeOfExpression(typeOf)) [typeOf, literal] = [literal, typeOf];
  if (!typeOf || !ts.isTypeOfExpression(typeOf) || !isBunIdentifier(unwrap(typeOf.expression))) return UNKNOWN;
  if (!literal || !(ts.isStringLiteral(literal) || ts.isNoSubstitutionTemplateLiteral(literal))) return UNKNOWN;
  const presentWhenEqual = literal.text !== "undefined";
  const presentWhenTrue = equal ? presentWhenEqual : !presentWhenEqual;
  return { whenTrue: presentWhenTrue, whenFalse: !presentWhenTrue };
}

function isBunIdentifier(node) {
  return !!node && ts.isIdentifier(node) && node.text === "Bun";
}

// leaves says whether a statement always leaves its block: return, throw,
// break or continue, alone or as the last statement of a block.
function leaves(statement) {
  if (!statement) return false;
  if (ts.isBlock(statement)) return leaves(statement.statements[statement.statements.length - 1]);
  return (
    ts.isReturnStatement(statement) ||
    ts.isThrowStatement(statement) ||
    ts.isBreakStatement(statement) ||
    ts.isContinueStatement(statement)
  );
}

function statementsOf(node) {
  if (ts.isBlock(node) || ts.isSourceFile(node) || ts.isModuleBlock(node) || ts.isCaseClause(node) || ts.isDefaultClause(node)) {
    return node.statements;
  }
  return undefined;
}

// guardedBy says whether child, a direct child of parent, runs only where a
// test parent makes has shown that Bun is present.
function guardedBy(parent, child) {
  if (ts.isIfStatement(parent)) {
    if (child === parent.thenStatement) return knowledge(parent.expression).whenTrue;
    if (child === parent.elseStatement) return knowledge(parent.expression).whenFalse;
    return false;
  }
  if (ts.isConditionalExpression(parent)) {
    if (child === parent.whenTrue) return knowledge(parent.condition).whenTrue;
    if (child === parent.whenFalse) return knowledge(parent.condition).whenFalse;
    return false;
  }
  if (ts.isBinaryExpression(parent) && child === parent.right) {
    const op = parent.operatorToken.kind;
    if (op === ts.SyntaxKind.AmpersandAmpersandToken) return knowledge(parent.left).whenTrue;
    if (op === ts.SyntaxKind.BarBarToken) return knowledge(parent.left).whenFalse;
    return false;
  }
  const statements = statementsOf(parent);
  if (statements) {
    for (const statement of statements) {
      if (statement === child) return false;
      if (ts.isIfStatement(statement) && knowledge(statement.expression).whenFalse && leaves(statement.thenStatement)) {
        return true;
      }
    }
  }
  return false;
}

// guarded says whether node runs only where Bun was shown to be present.
function guarded(node) {
  for (let child = node, parent = node.parent; parent; child = parent, parent = parent.parent) {
    // `typeof Bun` reads nothing; `typeof Bun.serve` reads a member of Bun.
    if (ts.isTypeOfExpression(parent)) {
      if (ts.isIdentifier(node) && unwrap(parent.expression) === node) return true;
    } else if (guardedBy(parent, child)) {
      return true;
    }
  }
  return false;
}

function isAmbient(statement) {
  return !!statement.modifiers?.some((modifier) => modifier.kind === ts.SyntaxKind.DeclareKeyword);
}

// bindsName says whether a binding name declares `Bun`.
function bindsName(name) {
  if (!name) return false;
  if (ts.isIdentifier(name)) return name.text === "Bun";
  if (ts.isObjectBindingPattern(name) || ts.isArrayBindingPattern(name)) {
    return name.elements.some((element) => !ts.isOmittedExpression(element) && bindsName(element.name));
  }
  return false;
}

// declaresBun says whether a scope node binds `Bun` itself. An ambient
// declaration binds nothing at run time.
function declaresBun(scope) {
  if ((ts.isFunctionExpression(scope) || ts.isClassExpression(scope)) && scope.name?.text === "Bun") return true;
  if (ts.isFunctionLike(scope) && scope.parameters?.some((parameter) => bindsName(parameter.name))) return true;
  if (ts.isCatchClause(scope)) return bindsName(scope.variableDeclaration?.name);
  if (ts.isForStatement(scope) || ts.isForInStatement(scope) || ts.isForOfStatement(scope)) {
    const init = scope.initializer;
    return !!init && ts.isVariableDeclarationList(init) && init.declarations.some((d) => bindsName(d.name));
  }
  const statements = ts.isBlock(scope) || ts.isSourceFile(scope) || ts.isModuleBlock(scope) ? scope.statements : undefined;
  if (!statements) return false;
  for (const statement of statements) {
    if (isAmbient(statement)) continue;
    if (ts.isVariableStatement(statement) && statement.declarationList.declarations.some((d) => bindsName(d.name))) return true;
    if ((ts.isFunctionDeclaration(statement) || ts.isClassDeclaration(statement) || ts.isEnumDeclaration(statement)) && statement.name?.text === "Bun") return true;
    if (ts.isImportDeclaration(statement) && statement.importClause) {
      const clause = statement.importClause;
      if (clause.isTypeOnly) continue;
      if (clause.name?.text === "Bun") return true;
      const bindings = clause.namedBindings;
      if (bindings && ts.isNamespaceImport(bindings) && bindings.name.text === "Bun") return true;
      if (bindings && ts.isNamedImports(bindings) && bindings.elements.some((e) => !e.isTypeOnly && e.name.text === "Bun")) return true;
    }
    if (ts.isImportEqualsDeclaration(statement) && !statement.isTypeOnly && statement.name.text === "Bun") return true;
  }
  return false;
}

function bound(node) {
  for (let parent = node.parent; parent; parent = parent.parent) {
    if (declaresBun(parent)) return true;
  }
  return false;
}

// isValueReference says whether the identifier `Bun` reads a binding: it is
// not a property or member name, a declared name, or part of a type or an
// ambient declaration.
function isValueReference(node) {
  const parent = node.parent;
  if (!parent) return false;
  if (ts.isPropertyAccessExpression(parent) && parent.name === node) return false;
  if (ts.isQualifiedName(parent)) return false;
  if (ts.isPropertyAssignment(parent) && parent.name === node) return false;
  if (ts.isMethodDeclaration(parent) || ts.isPropertyDeclaration(parent) || ts.isPropertySignature(parent) || ts.isMethodSignature(parent) || ts.isGetAccessor(parent) || ts.isSetAccessor(parent) || ts.isEnumMember(parent)) {
    if (parent.name === node) return false;
  }
  if (ts.isBindingElement(parent) && (parent.name === node || parent.propertyName === node)) return false;
  if (ts.isVariableDeclaration(parent) && parent.name === node) return false;
  if (ts.isParameter(parent) && parent.name === node) return false;
  if ((ts.isFunctionDeclaration(parent) || ts.isFunctionExpression(parent) || ts.isClassDeclaration(parent) || ts.isClassExpression(parent) || ts.isInterfaceDeclaration(parent) || ts.isTypeAliasDeclaration(parent) || ts.isEnumDeclaration(parent) || ts.isModuleDeclaration(parent)) && parent.name === node) return false;
  if (ts.isImportClause(parent) || ts.isImportSpecifier(parent) || ts.isNamespaceImport(parent) || ts.isImportEqualsDeclaration(parent)) return false;
  if (ts.isExportSpecifier(parent) && parent.name === node && parent.propertyName) return false;
  if (ts.isLabeledStatement(parent) || ts.isBreakOrContinueStatement(parent)) return false;
  if (ts.isJsxAttribute(parent)) return false;
  for (let current = parent; current; current = current.parent) {
    if (ts.isTypeNode(current)) return false;
    if (ts.isExpressionWithTypeArguments(current) && ts.isHeritageClause(current.parent) && current.parent.token === ts.SyntaxKind.ImplementsKeyword) return false;
    if (ts.isInterfaceDeclaration(current) || ts.isTypeAliasDeclaration(current)) return false;
    if (isAmbient(current)) return false;
  }
  return true;
}

function isGlobalThis(node) {
  node = unwrap(node);
  return !!node && ts.isIdentifier(node) && node.text === "globalThis";
}

// isGlobalBunDereference says whether node is `globalThis.Bun` (or
// `globalThis['Bun']`) whose member is then read without optional chaining.
function isGlobalBunDereference(node) {
  const namesBun =
    (ts.isPropertyAccessExpression(node) && node.name.text === "Bun") ||
    (ts.isElementAccessExpression(node) && ts.isStringLiteralLike(node.argumentExpression) && node.argumentExpression.text === "Bun");
  if (!namesBun || !isGlobalThis(node.expression)) return false;
  let outer = node;
  while (outer.parent && (ts.isParenthesizedExpression(outer.parent) || ts.isAsExpression(outer.parent) || ts.isNonNullExpression(outer.parent))) {
    outer = outer.parent;
  }
  const parent = outer.parent;
  if (!parent) return false;
  if ((ts.isPropertyAccessExpression(parent) || ts.isElementAccessExpression(parent)) && parent.expression === outer) {
    return !parent.questionDotToken;
  }
  if (ts.isCallExpression(parent) && parent.expression === outer) return !parent.questionDotToken;
  return false;
}

function isBunModule(specifier) {
  return specifier === "bun" || specifier.startsWith("bun:");
}

// importedBunModule returns the `bun` or `bun:` module a node imports at run
// time, or undefined.
function importedBunModule(node) {
  if (ts.isImportDeclaration(node)) {
    if (node.importClause?.isTypeOnly) return undefined;
    return ts.isStringLiteral(node.moduleSpecifier) && isBunModule(node.moduleSpecifier.text) ? node.moduleSpecifier.text : undefined;
  }
  if (ts.isExportDeclaration(node)) {
    if (node.isTypeOnly || !node.moduleSpecifier) return undefined;
    return ts.isStringLiteral(node.moduleSpecifier) && isBunModule(node.moduleSpecifier.text) ? node.moduleSpecifier.text : undefined;
  }
  if (ts.isImportEqualsDeclaration(node)) {
    if (node.isTypeOnly || !ts.isExternalModuleReference(node.moduleReference)) return undefined;
    const expression = node.moduleReference.expression;
    return ts.isStringLiteral(expression) && isBunModule(expression.text) ? expression.text : undefined;
  }
  if (ts.isCallExpression(node)) {
    const dynamic = node.expression.kind === ts.SyntaxKind.ImportKeyword;
    const required = ts.isIdentifier(node.expression) && node.expression.text === "require";
    const [argument] = node.arguments;
    if ((dynamic || required) && argument && ts.isStringLiteralLike(argument) && isBunModule(argument.text)) {
      return argument.text;
    }
  }
  return undefined;
}

function scan(sourceFile) {
  const uses = [];
  const imports = new Set();
  const position = (node) => {
    const { line, character } = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile));
    return { line: line + 1, column: character + 1 };
  };
  function visit(node) {
    if (ts.isIdentifier(node) && node.text === "Bun" && isValueReference(node) && !guarded(node) && !bound(node)) {
      uses.push(position(node));
    } else if ((ts.isPropertyAccessExpression(node) || ts.isElementAccessExpression(node)) && isGlobalBunDereference(node) && !guarded(node)) {
      uses.push(position(node));
    }
    const imported = importedBunModule(node);
    if (imported && !guarded(node)) imports.add(imported);
    ts.forEachChild(node, visit);
  }
  visit(sourceFile);
  return { uses, imports: [...imports].sort() };
}

const files = [];
const errors = [];
for (const file of input.files) {
  let text;
  try {
    text = readFileSync(path.resolve(input.root, file), "utf8");
  } catch (error) {
    errors.push({ path: file, message: String(error?.message ?? error) });
    continue;
  }
  if (!text.includes("Bun") && !text.includes("bun")) continue;
  const sourceFile = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, scriptKind(file));
  const { uses, imports } = scan(sourceFile);
  if (uses.length > 0 || imports.length > 0) files.push({ path: file, uses, imports });
}
process.stdout.write(JSON.stringify({ files, errors }));
