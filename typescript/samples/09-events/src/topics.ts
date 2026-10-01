import { ArrayOf, topic, Uuid } from '@putnami/events';

// Order processing pipeline — typed topic definitions

export const OrderCreated = topic('order.created', {
  orderId: Uuid,
  userId: String,
  items: ArrayOf({ productId: String, quantity: Number, price: Number }),
  total: Number,
});

export const InventoryReserved = topic('inventory.reserved', {
  orderId: Uuid,
  reservationId: Uuid,
  items: ArrayOf({ productId: String, quantity: Number }),
});

export const NotificationSent = topic('notification.sent', {
  orderId: Uuid,
  userId: String,
  channel: String,
});
