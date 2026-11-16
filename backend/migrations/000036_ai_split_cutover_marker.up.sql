CREATE TABLE ai_split_cutovers (
 run_id uuid PRIMARY KEY, status text NOT NULL CHECK(status='cutover_ready'),
 manifest_hash text NOT NULL, verified_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
