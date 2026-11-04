BEGIN;

DROP TRIGGER IF EXISTS outbox_delivery_insert_notify ON outbox_deliveries;
DROP FUNCTION IF EXISTS notify_outbox_delivery_insert();

COMMIT;
