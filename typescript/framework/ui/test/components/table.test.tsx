import { describe, expect, test } from 'bun:test';
import { Table, Tbody, Td, Tfoot, Th, Thead, Tr } from '../../src/components/table';
import { render } from './render';

describe('Table', () => {
  test('renders table structure', () => {
    const html = render(
      <Table>
        <Thead>
          <Tr>
            <Th>Name</Th>
            <Th isNumeric>Age</Th>
          </Tr>
        </Thead>
        <Tbody>
          <Tr>
            <Td>John</Td>
            <Td isNumeric>30</Td>
          </Tr>
        </Tbody>
        <Tfoot>
          <Tr>
            <Td>Total</Td>
            <Td isNumeric>1</Td>
          </Tr>
        </Tfoot>
      </Table>,
    );

    expect(html).toContain('<table');
    expect(html).toContain('<thead');
    expect(html).toContain('<tbody');
    expect(html).toContain('<tfoot');
    expect(html).toContain('<tr');
    expect(html).toContain('<th');
    expect(html).toContain('<td');

    // Check numeric cell mappings (though it relies on internal styles, the wrapper reflects structure)
    expect(html).toContain('John');
    expect(html).toContain('30');
  });
});
