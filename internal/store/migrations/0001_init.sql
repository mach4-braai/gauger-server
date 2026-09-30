CREATE TABLE repositories (
    full_name       text PRIMARY KEY,
    private         boolean,
    installation_id bigint,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE runs (
    id             bigint NOT NULL,
    attempt        integer NOT NULL,
    repository     text NOT NULL,
    workflow_name  text,
    head_branch    text,
    head_sha       text,
    event          text,
    status         text NOT NULL,
    conclusion     text,
    html_url       text,
    created_at     timestamptz,
    run_started_at timestamptz,
    updated_at     timestamptz,
    PRIMARY KEY (id, attempt)
);

CREATE TABLE jobs (
    id                bigint PRIMARY KEY,
    run_id            bigint NOT NULL,
    run_attempt       integer NOT NULL,
    repository        text NOT NULL,
    workflow_name     text,
    name              text,
    head_branch       text,
    status            text NOT NULL,
    conclusion        text,
    labels            text[] NOT NULL DEFAULT '{}',
    runner_name       text,
    runner_group_name text,
    html_url          text,
    created_at        timestamptz,
    started_at        timestamptz,
    completed_at      timestamptz,
    runner_seen_at    timestamptz,
    runner_done_at    timestamptz
);

CREATE INDEX jobs_run_idx ON jobs (run_id, run_attempt);
CREATE INDEX jobs_completed_idx ON jobs (completed_at);

CREATE TABLE steps (
    job_id       bigint NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    number       integer NOT NULL,
    name         text NOT NULL,
    status       text NOT NULL,
    conclusion   text,
    started_at   timestamptz,
    completed_at timestamptz,
    PRIMARY KEY (job_id, number)
);

CREATE INDEX steps_started_idx ON steps (started_at);

CREATE TABLE samples (
    job_id bigint NOT NULL,
    ts     timestamptz NOT NULL,
    metric text NOT NULL,
    series text NOT NULL DEFAULT '',
    value  double precision NOT NULL,
    PRIMARY KEY (job_id, metric, series, ts)
) PARTITION BY RANGE (ts);
