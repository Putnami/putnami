export interface Product {
  id: string;
  name: string;
  price: number;
  category: string;
}

export const products = new Map<string, Product>();

products.set('prod-1', { id: 'prod-1', name: 'Laptop', price: 999.99, category: 'electronics' });
products.set('prod-2', { id: 'prod-2', name: 'Headphones', price: 79.99, category: 'electronics' });
products.set('prod-3', { id: 'prod-3', name: 'Notebook', price: 12.99, category: 'office' });
products.set('prod-4', { id: 'prod-4', name: 'Pen Set', price: 24.99, category: 'office' });

// Simulate expensive computation
export function expensiveLookup(id: string): Product | undefined {
  // Simulate ~100ms delay
  const start = Date.now();
  while (Date.now() - start < 100) {
    // busy wait to simulate expensive computation
  }
  return products.get(id);
}
