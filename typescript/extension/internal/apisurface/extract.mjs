// Reads the exported declarations of TypeScript packages for the API check.
//
// Input, one JSON document on stdin:
//   { typescript, sides: [{ files: { "<path>": "<text>" }, entries: [{ key, file }] }] }
// Output, one JSON document on stdout:
//   { sides: [{ entries: [{ key, exports, referenced, stars, script }], errors }] }
//
// The script only parses: it loads the TypeScript parser from the directory
// `typescript` names, and never type-checks, reads a source file from disk or
// resolves a dependency. A side is a function of the file texts it is given.

import { createRequire } from "node:module";
import path from "node:path";

const input = JSON.parse(await Bun.stdin.text());
const ts = createRequire(import.meta.url)(input.typescript);

const SOURCE_EXTENSIONS = [".ts", ".tsx", ".d.ts", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"];
const JS_TO_TS = { ".js": [".ts", ".tsx"], ".jsx": [".tsx"], ".mjs": [".mts"], ".cjs": [".cts"] };

function scriptKind(file) {
  if (file.endsWith(".tsx")) return ts.ScriptKind.TSX;
  if (file.endsWith(".jsx")) return ts.ScriptKind.JSX;
  if (/\.[mc]?js$/.test(file)) return ts.ScriptKind.JS;
  return ts.ScriptKind.TS;
}

function readSide(side) {
  const files = new Map(Object.entries(side.files));
  const modules = new Map();
  const errors = [];
  const printer = ts.createPrinter({ removeComments: true, omitTrailingSemicolon: true });
  // sink collects the names a declaration's written types refer to, while the
  // declaration is read.
  let sink = null;

  // --- rendering -----------------------------------------------------------

  // print writes a node on one line.
  function print(node, sourceFile) {
    return printer.printNode(ts.EmitHint.Unspecified, node, sourceFile).replace(/\s+/g, " ").trim();
  }

  // extend returns scope plus the type parameters of a declaration, numbered
  // after the ones scope already holds.
  function extend(scope, typeParameters) {
    if (!typeParameters?.length) return scope;
    const inner = new Map(scope);
    let next = scope.count ?? 0;
    for (const parameter of typeParameters) inner.set(parameter.name.text, next++);
    inner.count = next;
    inner.parameters = scope.parameters;
    return inner;
  }

  // withParameters returns scope plus the positions of a signature's
  // parameters, which a type predicate names. A `this` parameter has none.
  function withParameters(scope, parameters) {
    const inner = new Map(scope);
    inner.count = scope.count ?? 0;
    inner.parameters = new Map();
    let index = 0;
    for (const parameter of parameters) {
      if (!ts.isIdentifier(parameter.name)) index++;
      else if (parameter.name.text !== "this") inner.parameters.set(parameter.name.text, index++);
    }
    return inner;
  }

  // normalize rewrites a type so that two spellings of one type print alike:
  // type parameters by position, no parameter names, string literals in one
  // quote style, T[] for Array<T>, and union and object members in one order.
  function normalize(node, scope, context, sourceFile) {
    const factory = context.factory;
    const visit = (child, inner = scope) => normalize(child, inner, context, sourceFile);
    const each = (target, inner = scope) => ts.visitEachChild(target, (child) => visit(child, inner), context);
    const sorted = (nodes) =>
      nodes
        .map((child) => ({ child, text: print(child, sourceFile) }))
        .sort((a, b) => (a.text < b.text ? -1 : a.text > b.text ? 1 : 0))
        .map((item) => item.child);
    const signatureParts = (signature) => {
      const inner = withParameters(extend(scope, signature.typeParameters), signature.parameters);
      const typeParameters = signature.typeParameters?.map((parameter) =>
        factory.updateTypeParameterDeclaration(
          parameter,
          undefined,
          factory.createIdentifier(`$${inner.get(parameter.name.text)}`),
          parameter.constraint && visit(parameter.constraint, inner),
          parameter.default && visit(parameter.default, inner),
        ),
      );
      const parameters = signature.parameters.map((parameter, index) =>
        factory.updateParameterDeclaration(
          parameter,
          undefined,
          parameter.dotDotDotToken,
          ts.isIdentifier(parameter.name) && parameter.name.text === "this"
            ? parameter.name
            : factory.createIdentifier(`_${index}`),
          parameter.questionToken,
          parameter.type && visit(parameter.type, inner),
          undefined,
        ),
      );
      return { typeParameters, parameters, type: signature.type && visit(signature.type, inner) };
    };

    if (ts.isTypeReferenceNode(node) && ts.isIdentifier(node.typeName)) {
      const name = node.typeName.text;
      if (scope.has(name) && !node.typeArguments) return factory.createTypeReferenceNode(`$${scope.get(name)}`);
      if (!scope.has(name)) sink?.add(name);
      if (name === "Array" && node.typeArguments?.length === 1) {
        return factory.createArrayTypeNode(visit(node.typeArguments[0]));
      }
    }
    if (ts.isExpressionWithTypeArguments(node) && ts.isIdentifier(node.expression)) sink?.add(node.expression.text);
    if (ts.isStringLiteral(node)) return factory.createStringLiteral(node.text);
    if (ts.isTypePredicateNode(node) && ts.isIdentifier(node.parameterName)) {
      // The predicate names a parameter: it is written by position.
      const position = scope.parameters?.get(node.parameterName.text);
      return factory.updateTypePredicateNode(
        node,
        node.assertsModifier,
        factory.createIdentifier(position === undefined ? "_" : `_${position}`),
        node.type && visit(node.type),
      );
    }
    if (ts.isFunctionTypeNode(node)) {
      const parts = signatureParts(node);
      return factory.updateFunctionTypeNode(node, parts.typeParameters, parts.parameters, parts.type);
    }
    if (ts.isConstructorTypeNode(node)) {
      const parts = signatureParts(node);
      return factory.updateConstructorTypeNode(node, node.modifiers, parts.typeParameters, parts.parameters, parts.type);
    }
    if (ts.isMethodSignature(node)) {
      const parts = signatureParts(node);
      return factory.updateMethodSignature(
        node,
        node.modifiers,
        node.name,
        node.questionToken,
        parts.typeParameters,
        parts.parameters,
        parts.type,
      );
    }
    if (ts.isCallSignatureDeclaration(node)) {
      const parts = signatureParts(node);
      return factory.updateCallSignature(node, parts.typeParameters, parts.parameters, parts.type);
    }
    if (ts.isConstructSignatureDeclaration(node)) {
      const parts = signatureParts(node);
      return factory.updateConstructSignature(node, parts.typeParameters, parts.parameters, parts.type);
    }
    if (ts.isUnionTypeNode(node)) {
      return factory.createUnionTypeNode(sorted(node.types.map((type) => visit(type))));
    }
    if (ts.isTypeLiteralNode(node)) {
      return factory.createTypeLiteralNode(sorted(node.members.map((member) => visit(member))));
    }
    return each(node);
  }

  // render writes a type, or any node made of types, in its normalized form.
  function render(node, scope, sourceFile) {
    const result = ts.transform(node, [(context) => (root) => normalize(root, scope, context, sourceFile)]);
    try {
      return print(result.transformed[0], sourceFile);
    } finally {
      result.dispose();
    }
  }

  function typeParametersOf(typeParameters, scope, sourceFile) {
    return (typeParameters ?? []).map((parameter) => ({
      constraint: parameter.constraint ? render(parameter.constraint, scope, sourceFile) : "",
      default: parameter.default ? render(parameter.default, scope, sourceFile) : "",
    }));
  }

  // signatureOf reads a function-like node. An unwritten type is "".
  function signatureOf(node, outer, sourceFile) {
    const scope = withParameters(extend(outer, node.typeParameters), node.parameters);
    const parameters = [];
    let thisType = "";
    for (const parameter of node.parameters) {
      if (ts.isIdentifier(parameter.name) && parameter.name.text === "this") {
        if (parameter.type) thisType = render(parameter.type, scope, sourceFile);
        continue;
      }
      parameters.push({
        type: parameter.type ? render(parameter.type, scope, sourceFile) : "",
        optional: Boolean(parameter.questionToken || parameter.initializer),
        rest: Boolean(parameter.dotDotDotToken),
      });
    }
    return {
      typeParams: typeParametersOf(node.typeParameters, scope, sourceFile),
      this: thisType,
      params: parameters,
      returns: node.type ? render(node.type, scope, sourceFile) : "",
    };
  }

  function lineOf(node, sourceFile) {
    return sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line + 1;
  }

  function hasModifier(node, kind) {
    return Boolean(node.modifiers?.some((modifier) => modifier.kind === kind));
  }

  function memberName(name, sourceFile) {
    if (!name) return "";
    if (ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNumericLiteral(name)) return name.text;
    return print(name, sourceFile);
  }

  // membersOf reads the members of a class, an interface or an object type.
  // A member written with `private` is kept by its name alone, so that a
  // member that becomes private is told from one that never was public; a
  // `#name` member is left out. Overloads of one method share a member.
  function membersOf(nodes, scope, sourceFile) {
    const members = new Map();
    const add = (name, node, fill) => {
      let member = members.get(name);
      if (!member) {
        member = {
          name,
          kind: "property",
          optional: false,
          abstract: false,
          readonly: false,
          access: "",
          type: "",
          line: lineOf(node, sourceFile),
        };
        members.set(name, member);
      }
      fill(member);
    };
    const addSignature = (member, kind, node, withBody) => {
      // An implementation that follows overload signatures is not callable.
      if (withBody && member.overloaded) return;
      member.kind = kind;
      (member.signatures ??= []).push(signatureOf(node, scope, sourceFile));
      if (!withBody) member.overloaded = true;
    };
    for (const node of nodes) {
      if (node.name && ts.isPrivateIdentifier(node.name)) continue;
      const prefix = hasModifier(node, ts.SyntaxKind.StaticKeyword) ? "static " : "";
      const name = ts.isConstructorDeclaration(node) ? "constructor" : prefix + memberName(node.name, sourceFile);
      if (hasModifier(node, ts.SyntaxKind.PrivateKeyword)) {
        add(name, node, (member) => {
          member.access = "private";
          if (ts.isConstructorDeclaration(node)) member.kind = "constructor";
        });
        continue;
      }
      const access = hasModifier(node, ts.SyntaxKind.ProtectedKeyword) ? "protected" : "";
      const flags = (member) => {
        member.optional = Boolean(node.questionToken);
        member.abstract = hasModifier(node, ts.SyntaxKind.AbstractKeyword);
        member.access = access;
      };
      if (ts.isPropertyDeclaration(node) || ts.isPropertySignature(node)) {
        add(name, node, (member) => {
          flags(member);
          member.readonly = hasModifier(node, ts.SyntaxKind.ReadonlyKeyword);
          member.type = node.type ? render(node.type, scope, sourceFile) : "";
        });
      } else if (ts.isMethodDeclaration(node) || ts.isMethodSignature(node)) {
        add(name, node, (member) => {
          flags(member);
          addSignature(member, "method", node, Boolean(node.body));
        });
      } else if (ts.isGetAccessorDeclaration(node)) {
        add(name, node, (member) => {
          flags(member);
          member.getter = true;
          if (node.type) member.type = render(node.type, scope, sourceFile);
        });
      } else if (ts.isSetAccessorDeclaration(node)) {
        add(name, node, (member) => {
          member.setter = true;
          const type = node.parameters[0]?.type;
          if (type && !member.type) member.type = render(type, scope, sourceFile);
        });
      } else if (ts.isConstructorDeclaration(node)) {
        add("constructor", node, (member) => {
          member.access = access;
          addSignature(member, "constructor", node, Boolean(node.body));
        });
        for (const parameter of node.parameters) {
          const isProperty = parameter.modifiers?.some((modifier) =>
            [
              ts.SyntaxKind.PublicKeyword,
              ts.SyntaxKind.ProtectedKeyword,
              ts.SyntaxKind.ReadonlyKeyword,
            ].includes(modifier.kind),
          );
          if (!isProperty || hasModifier(parameter, ts.SyntaxKind.PrivateKeyword)) continue;
          if (!ts.isIdentifier(parameter.name)) continue;
          add(parameter.name.text, parameter, (member) => {
            member.optional = Boolean(parameter.questionToken);
            member.readonly = hasModifier(parameter, ts.SyntaxKind.ReadonlyKeyword);
            member.access = hasModifier(parameter, ts.SyntaxKind.ProtectedKeyword) ? "protected" : "";
            member.type = parameter.type ? render(parameter.type, scope, sourceFile) : "";
          });
        }
      } else if (ts.isCallSignatureDeclaration(node)) {
        add("()", node, (member) => addSignature(member, "call signature", node, false));
      } else if (ts.isConstructSignatureDeclaration(node)) {
        add("new ()", node, (member) => addSignature(member, "construct signature", node, false));
      } else if (ts.isIndexSignatureDeclaration(node)) {
        const key = node.parameters[0]?.type ? render(node.parameters[0].type, scope, sourceFile) : "";
        add(`[${key}]`, node, (member) => {
          member.kind = "index signature";
          member.type = node.type ? render(node.type, scope, sourceFile) : "";
        });
      }
    }
    // A property with a getter and no setter cannot be written.
    return [...members.values()].map(({ overloaded, getter, setter, ...member }) => ({
      ...member,
      readonly: member.readonly || Boolean(getter && !setter),
    }));
  }

  // identity names a declaration node within the side.
  function identity(file, node) {
    return `${file}#${node.pos}`;
  }

  // basesOf returns, for each heritage clause of node, the identity of the
  // declaration it names, resolved in the scope of the declaring module; ""
  // for a base the package does not declare.
  const resolvingBases = new Set();
  function basesOf(node, module) {
    const bases = [];
    for (const clause of node.heritageClauses ?? []) {
      for (const type of clause.types) {
        let id = "";
        const name = ts.isIdentifier(type.expression) ? type.expression.text : undefined;
        const key = `${module.file}\0${name}`;
        if (name && module.locals.has(name) && !resolvingBases.has(key)) {
          resolvingBases.add(key);
          try {
            const found = resolveReference(module, { local: name }, name, new Set()).find(
              (declaration) => declaration.id && FOLLOWED.has(declaration.kind) && declaration.kind !== "enum",
            );
            if (found) id = found.id;
          } finally {
            resolvingBases.delete(key);
          }
        }
        bases.push(id);
      }
    }
    return bases;
  }

  function heritageOf(node, scope, sourceFile) {
    const heritage = [];
    for (const clause of node.heritageClauses ?? []) {
      const word = clause.token === ts.SyntaxKind.ImplementsKeyword ? "implements" : "extends";
      for (const type of clause.types) heritage.push(`${word} ${render(type, scope, sourceFile)}`);
    }
    return heritage;
  }

  // --- declarations --------------------------------------------------------

  const TYPE_SPACE = new Set(["interface", "type"]);

  function isFunctionExpression(node) {
    return Boolean(node) && (ts.isArrowFunction(node) || ts.isFunctionExpression(node));
  }

  function nodeKind(node) {
    if (ts.isFunctionDeclaration(node)) return "function";
    if (ts.isClassDeclaration(node)) return "class";
    if (ts.isInterfaceDeclaration(node)) return "interface";
    if (ts.isTypeAliasDeclaration(node)) return "type";
    if (ts.isEnumDeclaration(node)) return "enum";
    if (ts.isModuleDeclaration(node)) return "namespace";
    if (ts.isVariableDeclaration(node)) {
      return !node.type && isFunctionExpression(node.initializer) ? "function" : "variable";
    }
    return "variable";
  }

  // declarationsOf turns the declaration nodes of one name into its
  // declarations: one for the value the name stands for, and one for the type
  // when the same name also declares an interface or a type alias.
  function declarationsOf(name, nodes, module) {
    const sourceFile = module.sourceFile;
    const refs = new Set();
    const outer = sink;
    sink = refs;
    try {
      return readDeclarations(name, nodes, module, refs);
    } finally {
      sink = outer;
    }
  }

  // namespaceNames returns the names the namespace declarations among nodes
  // export.
  function namespaceNames(nodes) {
    const names = new Set();
    for (const node of nodes) {
      if (!ts.isModuleDeclaration(node) || !node.body || !ts.isModuleBlock(node.body)) continue;
      for (const statement of node.body.statements) {
        if (ts.isExportDeclaration(statement) && statement.exportClause && ts.isNamedExports(statement.exportClause)) {
          for (const element of statement.exportClause.elements) names.add(element.name.text);
        }
        if (!hasModifier(statement, ts.SyntaxKind.ExportKeyword)) continue;
        if (ts.isVariableStatement(statement)) {
          const bound = [];
          for (const declaration of statement.declarationList.declarations) bindingNames(declaration.name, bound);
          for (const each of bound) names.add(each);
        } else if (statement.name && ts.isIdentifier(statement.name)) names.add(statement.name.text);
      }
    }
    return [...names].sort();
  }

  function readDeclarations(name, nodes, module, refs) {
    const sourceFile = module.sourceFile;
    const groups = new Map();
    for (const node of nodes) {
      const kind = nodeKind(node);
      const space = TYPE_SPACE.has(kind) ? "type" : "value";
      if (!groups.has(space)) groups.set(space, []);
      groups.get(space).push({ node, kind });
    }
    const declarations = [];
    for (const group of groups.values()) {
      // A namespace merged with a function, a class or an enum adds members
      // the check does not read: the other declaration names the kind.
      const main = group.find((item) => item.kind !== "namespace") ?? group[0];
      const declaration = {
        name,
        kind: main.kind,
        file: module.file,
        line: lineOf(main.node, sourceFile),
        id: identity(module.file, main.node),
      };
      // source is what the reader follows references with; it is not written.
      Object.defineProperty(declaration, "source", { value: { refs, module, node: main.node } });
      const namespace = namespaceNames(group.map((item) => item.node));
      if (namespace.length) declaration.namespace = namespace;
      const same = group.filter((item) => item.kind === main.kind).map((item) => item.node);
      const empty = new Map();
      switch (main.kind) {
        case "function": {
          const functions = same.map((node) => (ts.isVariableDeclaration(node) ? node.initializer : node));
          const overloads = functions.filter((node) => !node.body);
          declaration.signatures = (overloads.length ? overloads : functions).map((node) =>
            signatureOf(node, empty, sourceFile),
          );
          break;
        }
        case "class":
        case "interface": {
          if (main.kind === "class") declaration.abstract = hasModifier(main.node, ts.SyntaxKind.AbstractKeyword);
          const scope = extend(empty, main.node.typeParameters);
          declaration.typeParams = typeParametersOf(main.node.typeParameters, scope, sourceFile);
          declaration.heritage = same.flatMap((node) => heritageOf(node, extend(empty, node.typeParameters), sourceFile));
          declaration.bases = same.flatMap((node) => basesOf(node, module));
          declaration.members = same.flatMap((node) =>
            membersOf(node.members, extend(empty, node.typeParameters), sourceFile),
          );
          break;
        }
        case "type": {
          const scope = extend(empty, main.node.typeParameters);
          declaration.typeParams = typeParametersOf(main.node.typeParameters, scope, sourceFile);
          if (ts.isTypeLiteralNode(main.node.type)) {
            declaration.members = membersOf(main.node.type.members, scope, sourceFile);
          } else {
            declaration.type = render(main.node.type, scope, sourceFile);
          }
          break;
        }
        case "enum":
          declaration.const = same.every((node) => hasModifier(node, ts.SyntaxKind.ConstKeyword));
          declaration.members = same.flatMap((node) => {
            // A member without a value takes the previous number plus one.
            let previous = "-1";
            return node.members.map((member) => {
              let value = "";
              if (member.initializer) value = render(member.initializer, empty, sourceFile);
              else if (/^-?\d+$/.test(previous)) value = String(Number(previous) + 1);
              previous = value;
              return {
                name: memberName(member.name, sourceFile),
                kind: "enum member",
                optional: false,
                abstract: false,
                readonly: false,
                access: "",
                type: value,
                line: lineOf(member, sourceFile),
              };
            });
          });
          break;
        case "variable":
          declaration.type = main.node.type ? render(main.node.type, empty, sourceFile) : "";
          break;
      }
      declarations.push(declaration);
    }
    return declarations;
  }

  // valueDeclaration reads an anonymous default export.
  function valueDeclaration(name, node, module) {
    const expression = ts.isExportAssignment(node) ? node.expression : node;
    const declaration = {
      name,
      kind: "variable",
      file: module.file,
      line: lineOf(node, module.sourceFile),
      id: identity(module.file, expression),
    };
    const refs = new Set();
    Object.defineProperty(declaration, "source", { value: { refs, module, node: expression } });
    const outer = sink;
    sink = refs;
    try {
      readValue(declaration, expression, module);
    } finally {
      sink = outer;
    }
    return declaration;
  }

  function readValue(declaration, expression, module) {
    if (isFunctionExpression(expression) || ts.isFunctionDeclaration(expression)) {
      declaration.kind = "function";
      declaration.signatures = [signatureOf(expression, new Map(), module.sourceFile)];
    } else if (ts.isClassDeclaration(expression) || ts.isClassExpression(expression)) {
      const scope = extend(new Map(), expression.typeParameters);
      declaration.kind = "class";
      declaration.abstract = hasModifier(expression, ts.SyntaxKind.AbstractKeyword);
      declaration.typeParams = typeParametersOf(expression.typeParameters, scope, module.sourceFile);
      declaration.heritage = heritageOf(expression, scope, module.sourceFile);
      declaration.bases = basesOf(expression, module);
      declaration.members = membersOf(expression.members, scope, module.sourceFile);
    }
  }

  // --- modules -------------------------------------------------------------

  function bindingNames(name, into) {
    if (ts.isIdentifier(name)) into.push(name.text);
    else for (const element of name.elements) if (!ts.isOmittedExpression(element)) bindingNames(element.name, into);
  }

  function loadModule(file) {
    if (modules.has(file)) return modules.get(file);
    const sourceFile = ts.createSourceFile(file, files.get(file), ts.ScriptTarget.Latest, true, scriptKind(file));
    for (const diagnostic of sourceFile.parseDiagnostics ?? []) {
      errors.push({
        file,
        line: sourceFile.getLineAndCharacterOfPosition(diagnostic.start ?? 0).line + 1,
        message: ts.flattenDiagnosticMessageText(diagnostic.messageText, " "),
      });
    }
    const module = { file, sourceFile, locals: new Map(), exports: new Map(), stars: [], script: true };
    // explicit says the file writes what it exports, with `export { … }`,
    // `export *` or an export assignment.
    let explicit = false;
    modules.set(file, module);
    const declare = (name, node) => {
      const local = module.locals.get(name);
      if (local?.nodes) local.nodes.push(node);
      else module.locals.set(name, { nodes: [node] });
    };
    for (const statement of sourceFile.statements) {
      // A file with no import and no export is a script or a CommonJS module:
      // it has no exports this reader knows.
      if (
        ts.isImportDeclaration(statement) ||
        ts.isExportDeclaration(statement) ||
        ts.isExportAssignment(statement) ||
        ts.isImportEqualsDeclaration(statement) ||
        hasModifier(statement, ts.SyntaxKind.ExportKeyword)
      ) {
        module.script = false;
      }
      if (ts.isImportDeclaration(statement)) {
        const from = statement.moduleSpecifier.text;
        const clause = statement.importClause;
        if (!clause) continue;
        const typeOnly = Boolean(clause.isTypeOnly ?? clause.phaseModifier === ts.SyntaxKind.TypeKeyword);
        if (clause.name) module.locals.set(clause.name.text, { from, name: "default", typeOnly });
        const bindings = clause.namedBindings;
        if (bindings && ts.isNamespaceImport(bindings)) module.locals.set(bindings.name.text, { from, name: "*" });
        else if (bindings) {
          for (const element of bindings.elements) {
            module.locals.set(element.name.text, {
              from,
              name: (element.propertyName ?? element.name).text,
              typeOnly: typeOnly || Boolean(element.isTypeOnly),
            });
          }
        }
      } else if (ts.isExportDeclaration(statement)) {
        explicit = true;
        const from = statement.moduleSpecifier?.text;
        const clause = statement.exportClause;
        if (!clause) module.stars.push(from);
        else if (ts.isNamespaceExport(clause)) module.exports.set(clause.name.text, { from, name: "*" });
        else {
          for (const element of clause.elements) {
            const source = (element.propertyName ?? element.name).text;
            const typeOnly = Boolean(statement.isTypeOnly || element.isTypeOnly);
            module.exports.set(element.name.text, from ? { from, name: source, typeOnly } : { local: source, typeOnly });
          }
        }
      } else if (ts.isImportEqualsDeclaration(statement)) {
        // `export import A = N.A` exports a name the reader does not follow.
        if (hasModifier(statement, ts.SyntaxKind.ExportKeyword)) {
          module.exports.set(statement.name.text, { unknown: true });
        }
      } else if (ts.isExportAssignment(statement)) {
        explicit = true;
        // `export =` is the CommonJS form: what it assigns is not read.
        if (statement.isExportEquals) {
          module.exports.set("export =", { unknown: true });
          module.script = true;
          module.exportEquals = true;
        }
        else if (ts.isIdentifier(statement.expression)) module.exports.set("default", { local: statement.expression.text });
        else module.exports.set("default", { node: statement });
      } else {
        const exported = hasModifier(statement, ts.SyntaxKind.ExportKeyword);
        const isDefault = hasModifier(statement, ts.SyntaxKind.DefaultKeyword);
        if (ts.isVariableStatement(statement)) {
          for (const declaration of statement.declarationList.declarations) {
            const names = [];
            bindingNames(declaration.name, names);
            for (const name of names) {
              // A destructured name has no declaration of its own to read.
              declare(name, ts.isIdentifier(declaration.name) ? declaration : declaration.name);
              if (exported) module.exports.set(name, { local: name });
            }
          }
        } else if (
          ts.isFunctionDeclaration(statement) ||
          ts.isClassDeclaration(statement) ||
          ts.isInterfaceDeclaration(statement) ||
          ts.isTypeAliasDeclaration(statement) ||
          ts.isEnumDeclaration(statement) ||
          (ts.isModuleDeclaration(statement) && ts.isIdentifier(statement.name) && statement.name.text !== "global")
        ) {
          const name = statement.name?.text;
          if (name) declare(name, statement);
          if (!exported) continue;
          if (isDefault) module.exports.set("default", name ? { local: name } : { node: statement });
          else if (name) module.exports.set(name, { local: name });
        }
      }
    }
    if (module.exportEquals) module.script = true;
    // A declaration file that is a module and writes no export list exports
    // every top-level declaration.
    if (/\.d\.[mc]?ts$/.test(file) && !module.script && !explicit) {
      for (const [name, local] of module.locals) {
        if (local.nodes && !module.exports.has(name)) module.exports.set(name, { local: name });
      }
    }
    return module;
  }

  // resolveModule returns the file a relative specifier names, or undefined
  // for another package or a file the side does not hold.
  function resolveModule(fromFile, specifier) {
    if (!specifier?.startsWith(".")) return undefined;
    const base = path.posix.normalize(path.posix.join(path.posix.dirname(fromFile), specifier));
    const candidates = [base];
    const extension = path.posix.extname(base);
    for (const replacement of JS_TO_TS[extension] ?? []) candidates.push(base.slice(0, -extension.length) + replacement);
    for (const suffix of SOURCE_EXTENSIONS) candidates.push(base + suffix);
    for (const suffix of SOURCE_EXTENSIONS) candidates.push(`${base}/index${suffix}`);
    return candidates.find((candidate) => files.has(candidate));
  }

  function outside(name, from, importedName) {
    return [{ name, kind: "external", type: `${from}#${importedName}` }];
  }

  // resolveExport returns the declarations a module exports under name, as the
  // exported name exportedAs.
  function resolveExport(module, name, exportedAs, seen) {
    const key = `${module.file}\0${name}`;
    if (seen.has(key)) return [];
    seen.add(key);
    const entry = module.exports.get(name);
    if (entry) return resolveReference(module, entry, exportedAs, seen);
    if (name === "default") return [];
    for (const from of module.stars) {
      const target = resolveModule(module.file, from);
      if (!target) continue;
      const found = resolveExport(loadModule(target), name, exportedAs, seen);
      if (found.length) return found;
    }
    return [];
  }

  function resolveImport(module, from, importedName, exportedAs, seen) {
    const target = resolveModule(module.file, from);
    if (!target) return outside(exportedAs, from, importedName);
    if (importedName === "*") return [{ name: exportedAs, kind: "namespace", file: target, line: 1 }];
    const found = resolveExport(loadModule(target), importedName, exportedAs, seen);
    return found.length ? found : [{ name: exportedAs, kind: "unknown" }];
  }

  // typeOnly marks declarations that an `export type` or an `import type`
  // carries: the name stands for a type and no longer for a value.
  function typeOnly(declarations, marked) {
    if (marked) for (const declaration of declarations) declaration.typeOnly = true;
    return declarations;
  }

  function resolveReference(module, reference, exportedAs, seen) {
    if (reference.unknown) return [{ name: exportedAs, kind: "unknown" }];
    if (reference.node) return [valueDeclaration(exportedAs, reference.node, module)];
    if (reference.from) {
      return typeOnly(resolveImport(module, reference.from, reference.name, exportedAs, seen), reference.typeOnly);
    }
    const local = module.locals.get(reference.local);
    if (!local) return [{ name: exportedAs, kind: "unknown" }];
    if (local.from) {
      return typeOnly(
        resolveImport(module, local.from, local.name, exportedAs, seen),
        reference.typeOnly || local.typeOnly,
      );
    }
    return typeOnly(declarationsOf(exportedAs, local.nodes, module), reference.typeOnly);
  }

  // FOLLOWED are the kinds of declaration whose members a reference reaches.
  const FOLLOWED = new Set(["interface", "type", "class", "enum"]);

  // referencedBy returns the declarations of the package that the written
  // types of the exported ones name without exporting them: a return type, a
  // parameter type, a base class, a member type. It follows them in turn, in
  // the files of the package only, and reads each declaration once. Two
  // declarations of one name in two files are both kept: each has its file.
  function referencedBy(exported) {
    const seen = new Set(exported.map((declaration) => declaration.source?.node).filter(Boolean));
    const byKey = new Map();
    const queue = [...exported];
    while (queue.length) {
      const source = queue.shift().source;
      if (!source) continue;
      for (const name of source.refs) {
        if (!source.module.locals.has(name)) continue;
        for (const found of resolveReference(source.module, { local: name }, name, new Set())) {
          const node = found.source?.node;
          if (!node || !FOLLOWED.has(found.kind) || seen.has(node)) continue;
          seen.add(node);
          byKey.set(`${found.name} ${found.file} ${found.id}`, found);
          queue.push(found);
        }
      }
    }
    return [...byKey.entries()]
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([, declaration]) => declaration);
  }

  // exportedNames collects every name a module exports, and the specifiers of
  // the `export *` it cannot follow.
  function exportedNames(module, names, stars, seen) {
    if (seen.has(module.file)) return;
    seen.add(module.file);
    for (const name of module.exports.keys()) names.add(name);
    for (const from of module.stars) {
      const target = resolveModule(module.file, from);
      if (!target) {
        stars.add(from.startsWith(".") ? path.posix.normalize(path.posix.join(path.posix.dirname(module.file), from)) : from);
        continue;
      }
      const inner = new Set();
      exportedNames(loadModule(target), inner, stars, seen);
      inner.delete("default");
      for (const name of inner) names.add(name);
    }
  }

  const entries = side.entries.map((entry) => {
    if (!files.has(entry.file)) return { key: entry.key, exports: [], referenced: [], stars: [], script: false };
    const module = loadModule(entry.file);
    const names = new Set();
    const stars = new Set();
    exportedNames(module, names, stars, new Set());
    const exports = [...names].sort().flatMap((name) => resolveExport(module, name, name, new Set()));
    return {
      key: entry.key,
      exports,
      referenced: referencedBy(exports),
      stars: [...stars].sort(),
      script: module.script,
    };
  });
  return { entries, errors };
}

process.stdout.write(JSON.stringify({ sides: input.sides.map(readSide) }));
