BEGIN;

CREATE OR REPLACE FUNCTION notify_outbox_delivery_insert() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_notify('bx_outbox_delivery_ready', '');
    RETURN NULL;
END;
$$;

CREATE TRIGGER outbox_delivery_insert_notify
AFTER INSERT ON outbox_deliveries
FOR EACH STATEMENT EXECUTE FUNCTION notify_outbox_delivery_insert();

COMMENT ON FUNCTION notify_outbox_delivery_insert() IS
    'Best-effort wakeup signal for Outbox relays; persisted delivery rows remain authoritative.';

COMMIT;
