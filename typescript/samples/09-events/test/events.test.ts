import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { api } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { events } from '@putnami/events';
import reserveInventory from '../src/events/reserve-inventory.on';
import sendNotification from '../src/events/send-notification.on';
import { clearEvents } from '../src/store';

describe('events sample', () => {
  let testApp: TestApp;

  beforeAll(async () => {
    clearEvents();

    testApp = await createTestApp({
      plugins: [
        api({ scanPath: 'src/api' }),
        events({ autoScan: false, handlers: [reserveInventory, sendNotification] }),
      ],
    });
  });

  afterAll(async () => {
    await testApp.stop();
  });

  it('should create an order and trigger event pipeline', async () => {
    const res = await testApp.fetch('/orders', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        userId: 'user-1',
        items: [{ productId: 'prod-1', quantity: 2, price: 25.0 }],
      }),
    });

    expect(res.status).toBe(201);
    const data = await res.json();
    expect(data.orderId).toBeDefined();
    expect(data.total).toBe(50);
  });

  it('should return events on demand via GET /events', async () => {
    // Wait for async event handlers to complete the pipeline
    await Bun.sleep(500);

    const res = await testApp.fetch('/events');
    expect(res.status).toBe(200);

    const data = await res.json();
    expect(data.events).toBeArray();
    expect(data.total).toBeGreaterThanOrEqual(3);

    // Verify the full pipeline ran: order.created → inventory.reserved → notification.sent
    const topics = data.events.map((e: { topic: string }) => e.topic);
    expect(topics).toContain('order.created');
    expect(topics).toContain('inventory.reserved');
    expect(topics).toContain('notification.sent');
  });
});
