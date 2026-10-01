import { endpoint, json } from '@putnami/application';
import { ArrayOf, getPublisher } from '@putnami/events';
import { record } from '../../store';
import { OrderCreated } from '../../topics';

// Publish pattern: POST creates an order and publishes order.created.
// Handlers consume the event on the flow (reactive, push-based).

const publishOrderCreated = getPublisher(OrderCreated);

export const POST = endpoint()
  .body({
    userId: String,
    items: ArrayOf({
      productId: String,
      quantity: Number,
      price: Number,
    }),
  })
  .handle(async (ctx) => {
    const body = await ctx.body();

    if (!body?.userId || !Array.isArray(body.items) || body.items.length === 0) {
      return json({ error: 'userId and items are required' }, { status: 400 });
    }

    const items = body.items as Array<{ productId: string; quantity: number; price: number }>;
    const orderId = crypto.randomUUID();
    const total = items.reduce((sum, item) => sum + item.price * item.quantity, 0);

    const payload = { orderId, userId: body.userId, items, total };

    // Publish triggers the pipeline on the flow: order.created → inventory.reserved → notification.sent
    await publishOrderCreated(payload);
    record(OrderCreated.name, payload);

    return json({ orderId, total, message: 'Order created — event pipeline triggered' }, { status: 201 });
  });
