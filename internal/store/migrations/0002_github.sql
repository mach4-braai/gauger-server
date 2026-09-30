CREATE TABLE app_config (
    id             integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    app_id         bigint NOT NULL,
    slug           text NOT NULL,
    html_url       text NOT NULL,
    client_id      text NOT NULL,
    client_secret  text NOT NULL,
    webhook_secret text NOT NULL,
    private_key    text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    id          text PRIMARY KEY,
    event       text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webhook_deliveries_received_idx ON webhook_deliveries (received_at);

CREATE TABLE tasks (
    kind       text NOT NULL,
    key        text NOT NULL,
    repository text NOT NULL,
    attempts   integer NOT NULL DEFAULT 0,
    next_at    timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    last_error text,
    PRIMARY KEY (kind, key)
);

CREATE INDEX tasks_next_idx ON tasks (next_at);
