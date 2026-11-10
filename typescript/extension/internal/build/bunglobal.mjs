// Finds the references to the Bun global that a module makes outside a
// `typeof Bun` guard, for the node-target check of the build.
//
// Input, one JSON document on stdin:
//   { typescript, root, files: ["<path relative to root>"] }
// Output, one JSON document on stdout:
//   { files: [{ path, uses: [{ line, column }] }], errors: [{ path, message }] }
//
// The script only parses: it loads the TypeScript parser from the directory
// `typescript` names and never type-checks or resolves a module. A reference
// is an identifier `Bun` in a value position that no enclosing scope declares.
// It is guarded when it is the operand of `typeof`, or when it lies in a branch
// of an `if`, a conditional expression or a logical expression whose condition
// tests `typeof Bun`, or after an `if` that tests `typeof Bun` and leaves the
// block. Either branch counts: the module considered the absence of Bun.

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

// testsTypeofBun says whether an expression contains `typeof Bun`.
function testsTypeofBun(node) {
  if (!node) return false;
  if (ts.isTypeOfExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === "Bun") return true;
  return ts.forEachChild(node, testsTypeofBun) === true;
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

// guardedBy says whether child, a direct child of parent, runs only under a
// `typeof Bun` test that parent makes.
function guardedBy(parent, child) {
  if (ts.isIfStatement(parent)) {
    return child !== parent.expression && testsTypeofBun(parent.expression);
  }
  if (ts.isConditionalExpression(parent)) {
    return child !== parent.condition && testsTypeofBun(parent.condition);
  }
  if (ts.isBinaryExpression(parent)) {
    const op = parent.operatorToken.kind;
    const logical =
      op === ts.SyntaxKind.AmpersandAmpersandToken ||
      op === ts.SyntaxKind.BarBarToken ||
      op === ts.SyntaxKind.QuestionQuestionToken;
    return logical && child === parent.right && testsTypeofBun(parent.left);
  }
  const statements = ts.isBlock(parent) || ts.isSourceFile(parent) || ts.isModuleBlock(parent) || ts.isCaseClause(parent) || ts.isDefaultClause(parent)
    ? parent.statements
    : undefined;
  if (statements) {
    for (const statement of statements) {
      if (statement === child) return false;
      if (ts.isIfStatement(statement) && !statement.elseStatement && testsTypeofBun(statement.expression) && leaves(statement.thenStatement)) {
        return true;
      }
    }
  }
  return false;
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

// declaresBun says whether a scope node declares `Bun` itself.
function declaresBun(scope) {
  if (ts.isFunctionLike(scope)) {
    if (scope.parameters?.some((parameter) => bindsName(parameter.name))) return true;
    if ((ts.isFunctionExpression(scope) || ts.isClassExpression(scope)) && scope.name?.text === "Bun") return true;
  }
  if (ts.isCatchClause(scope)) return bindsName(scope.variableDeclaration?.name);
  if (ts.isForStatement(scope) || ts.isForInStatement(scope) || ts.isForOfStatement(scope)) {
    const init = scope.initializer;
    return !!init && ts.isVariableDeclarationList(init) && init.declarations.some((d) => bindsName(d.name));
  }
  const statements = ts.isBlock(scope) || ts.isSourceFile(scope) || ts.isModuleBlock(scope) ? scope.statements : undefined;
  if (!statements) return false;
  for (const statement of statements) {
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
    if (ts.isImportEqualsDeclaration(statement) && statement.name.text === "Bun") return true;
  }
  return false;
}

// isValueReference says whether the identifier `Bun` reads a binding: it is
// not a property or member name, a declared name, or part of a type.
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
    if (ts.isTypeNode(current) || ts.isExpressionWithTypeArguments(current) && ts.isHeritageClause(current.parent) && current.parent.token === ts.SyntaxKind.ImplementsKeyword) return false;
    if (ts.isInterfaceDeclaration(current) || ts.isTypeAliasDeclaration(current)) return false;
    if (ts.isModuleDeclaration(current) && current.modifiers?.some((m) => m.kind === ts.SyntaxKind.DeclareKeyword)) return false;
  }
  return true;
}

function unguardedUses(sourceFile) {
  const uses = [];
  function visit(node) {
    if (ts.isIdentifier(node) && node.text === "Bun" && isValueReference(node)) {
      let guarded = false;
      let bound = false;
      for (let child = node, parent = node.parent; parent; child = parent, parent = parent.parent) {
        if (ts.isTypeOfExpression(parent) && child === node) {
          guarded = true;
          break;
        }
        if (guardedBy(parent, child)) {
          guarded = true;
          break;
        }
        if (declaresBun(parent)) {
          bound = true;
          break;
        }
      }
      if (!guarded && !bound) {
        const { line, character } = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile));
        uses.push({ line: line + 1, column: character + 1 });
      }
    }
    ts.forEachChild(node, visit);
  }
  visit(sourceFile);
  return uses;
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
  if (!text.includes("Bun")) continue;
  const sourceFile = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, scriptKind(file));
  const uses = unguardedUses(sourceFile);
  if (uses.length > 0) files.push({ path: file, uses });
}
process.stdout.write(JSON.stringify({ files, errors }));
