import { describe, expect, test } from 'bun:test';
import { operationTypeBase } from '../../src/generator/string-utils';

describe('operationTypeBase', () => {
  // "/.well-known/putnami/events" yields the canonical operation id
  // "get.well-known_Putnami_Events", and `pascalCase` alone left the dot in the
  // emitted type names ("Get.wellKnownPutnamiEventsResult"), which Biome
  // refuses for the whole generated client. go/framework/api's
  // `upperCamelClient` splits the same segment the same way.
  test('treats every non-alphanumeric character as a word break', () => {
    const cases: Record<string, string> = {
      'get.well-known_Putnami_Events': 'GetWellKnownPutnamiEvents',
      listUsers: 'ListUsers',
      getUsers_Id: 'GetUsersId',
      'getV1_Operator_Cli-usage': 'GetV1OperatorCliUsage',
      'get.well-known': 'GetWellKnown',
      'list users (all)': 'ListUsersAll',
      '2faCodes': '_2faCodes',
      '...': 'Operation',
      '': 'Operation',
    };
    for (const [operationId, base] of Object.entries(cases)) {
      expect({ operationId, base: operationTypeBase(operationId) }).toEqual({ operationId, base });
    }
  });
});
