-- Transport ownership cannot safely be rolled back while RabbitMQ holds tasks.
-- Keep the audit table and require an explicit drain/reconciliation procedure.
DO $$ BEGIN RAISE EXCEPTION 'RabbitMQ cutover requires a drained broker and explicit reconciliation; automatic downgrade disabled'; END $$;
