export interface Item {
  id: string;
  name: string;
  price: number;
  stock: number;
  /** Optional: absent means "still sold". */
  discontinuedAt?: string;
}

export const items = new Map<string, Item>();

items.set('item-1', { id: 'item-1', name: 'Widget', price: 9.99, stock: 100 });
items.set('item-2', { id: 'item-2', name: 'Gadget', price: 24.99, stock: 50, discontinuedAt: '2026-01-31' });
items.set('item-3', { id: 'item-3', name: 'Doohickey', price: 4.99, stock: 200 });

/** The ids of the seeded catalog; an item created later carries a generated id. */
export const SEEDED_ITEM_IDS: readonly string[] = [...items.keys()];
