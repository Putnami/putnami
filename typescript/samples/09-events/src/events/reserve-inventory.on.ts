import { getPublisher, handler } from '@putnami/events';
import { record } from '../store';
import { InventoryReserved, OrderCreated } from '../topics';

// On the flow: reacts to order.created → publishes inventory.reserved

const publishInventoryReserved = getPublisher(InventoryReserved);

export default handler(OrderCreated).handle(async (msg) => {
  const { orderId, items } = msg.payload;

  const payload = {
    orderId,
    reservationId: crypto.randomUUID(),
    items: items.map((i) => ({ productId: i.productId, quantity: i.quantity })),
  };

  await publishInventoryReserved(payload);
  record(InventoryReserved.name, payload);
});
