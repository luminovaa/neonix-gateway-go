CREATE TABLE IF NOT EXISTS neonix_proxy_log_view_state (
    id          SMALLINT PRIMARY KEY CHECK (id = 1),
    cleared_at  TIMESTAMPTZ NOT NULL DEFAULT '-infinity'::timestamptz
);

INSERT INTO neonix_proxy_log_view_state (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
