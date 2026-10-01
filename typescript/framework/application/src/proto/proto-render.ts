// ---------------------------------------------------------------------------
// Proto3 text rendering — produces .proto source text from generation context
// ---------------------------------------------------------------------------

import type { GenerationContext, MessageField, RpcEntry } from './proto-types';

/**
 * Render the complete proto3 source text.
 */
export function renderProto(ctx: GenerationContext): string {
  const lines: string[] = [];

  lines.push('syntax = "proto3";');
  lines.push('');
  lines.push(`package ${ctx.options.packageName};`);

  if (ctx.options.goPackage) {
    lines.push('');
    lines.push(`option go_package = "${ctx.options.goPackage}";`);
  }

  // Render enums
  if (ctx.enums.size > 0) {
    lines.push('');
    for (const [name, values] of ctx.enums) {
      renderEnum(lines, name, values);
      lines.push('');
    }
  }

  // Render messages
  if (ctx.messages.size > 0) {
    lines.push('');
    for (const [name, fields] of ctx.messages) {
      renderMessage(lines, name, fields);
      lines.push('');
    }
  }

  // Render services
  for (const [name, rpcs] of ctx.services) {
    renderService(lines, name, rpcs);
    lines.push('');
  }

  return lines.join('\n');
}

function renderMessage(lines: string[], name: string, fields: MessageField[]): void {
  lines.push(`message ${name} {`);
  for (const field of fields) {
    const comment = field.comment ? ` // ${field.comment}` : '';
    if (field.mapKeyType && field.mapValueType) {
      // Map field: map<KeyType, ValueType> name = number;
      lines.push(`  map<${field.mapKeyType}, ${field.mapValueType}> ${field.name} = ${field.number};${comment}`);
    } else {
      const prefix = field.repeated ? 'repeated ' : field.optional ? 'optional ' : '';
      lines.push(`  ${prefix}${field.type} ${field.name} = ${field.number};${comment}`);
    }
  }
  lines.push('}');
}

function renderEnum(lines: string[], name: string, values: string[]): void {
  lines.push(`enum ${name} {`);
  // Proto3 requires the first value to be 0 (unspecified sentinel)
  const prefix = toScreamingSnakeCase(name);
  lines.push(`  ${prefix}_UNSPECIFIED = 0;`);
  for (let i = 0; i < values.length; i++) {
    lines.push(`  ${prefix}_${toScreamingSnakeCase(values[i])} = ${i + 1};`);
  }
  lines.push('}');
}

function renderService(lines: string[], name: string, rpcs: RpcEntry[]): void {
  lines.push(`service ${name} {`);
  for (const rpc of rpcs) {
    const req = rpc.clientStreaming ? `stream ${rpc.requestMessage}` : rpc.requestMessage;
    const res = rpc.serverStreaming ? `stream ${rpc.responseMessage}` : rpc.responseMessage;
    lines.push(`  rpc ${rpc.name}(${req}) returns (${res});`);
  }
  lines.push('}');
}

// ---------------------------------------------------------------------------
// String utilities
// ---------------------------------------------------------------------------

export function pascalCase(str: string): string {
  return str
    .split(/[-_]/)
    .map((s) => s.charAt(0).toUpperCase() + s.slice(1))
    .join('');
}

export function toSnakeCase(str: string): string {
  return str.replace(/([a-z])([A-Z])/g, '$1_$2').toLowerCase();
}

/**
 * The `json_name` protobuf assigns a field: lower camel case of its declared
 * name. This is the emitter's rule, and the only one — a descriptor consumer
 * reads `jsonName` off the field rather than re-deriving it, so the JSON key a
 * Connect payload carries cannot differ from the one the `.proto` declares.
 */
export function protoJsonName(fieldName: string): string {
  return fieldName.replace(/_+([a-z0-9])/g, (_match, character: string) => character.toUpperCase()).replace(/_+$/, '');
}

export function toScreamingSnakeCase(str: string): string {
  return str
    .replace(/([a-z])([A-Z])/g, '$1_$2')
    .replace(/[-\s]+/g, '_')
    .toUpperCase();
}
