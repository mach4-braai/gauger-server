ALTER TABLE jobs ADD COLUMN artifact_ingested_at timestamptz;

CREATE INDEX jobs_runner_idx ON jobs (run_id, run_attempt, runner_name);
