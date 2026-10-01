import { getPublisher, handler } from '@putnami/events';
import { record } from '../store';
import { InventoryReserved, NotificationSent } from '../topics';

// On the flow: reacts to inventory.reserved → publishes notification.sent

const publishNotificationSent = getPublisher(NotificationSent);

export default handler(InventoryReserved).handle(async (msg) => {
  const { orderId } = msg.payload;

  const payload = {
    orderId,
    userId: 'user-from-order', // in a real app, look up from order service
    channel: 'email',
  };

  await publishNotificationSent(payload);
  record(NotificationSent.name, payload);
});
