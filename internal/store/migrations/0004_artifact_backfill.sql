INSERT INTO tasks (kind, key, repository, next_at, expires_at)
SELECT 'run', run_id::text || ':' || run_attempt::text, min(repository), now(), max(completed_at) + interval '7 days'
FROM jobs
WHERE status = 'completed' AND completed_at > now() - interval '7 days'
  AND runner_done_at IS NULL AND artifact_ingested_at IS NULL
GROUP BY run_id, run_attempt
ON CONFLICT DO NOTHING;
