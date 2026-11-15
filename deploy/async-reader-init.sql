-- psql variables come from environment inside the init container. No passwords
-- are printed, and existing business data is untouched.
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'reader', :'reader_password')
WHERE NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=:'reader') \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO %I', current_database(), :'reader') \gexec
SELECT format('GRANT USAGE ON SCHEMA public TO %I', :'reader') \gexec
SELECT format('GRANT SELECT ON ALL TABLES IN SCHEMA public TO %I', :'reader') \gexec
SELECT format('ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO %I', :'reader') \gexec
