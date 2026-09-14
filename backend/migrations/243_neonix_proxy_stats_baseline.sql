CREATE TABLE IF NOT EXISTS neonix_proxy_stats_baseline (
    id                SMALLINT PRIMARY KEY CHECK (id = 1),
    success_requests  BIGINT NOT NULL DEFAULT 0,
    failed_requests   BIGINT NOT NULL DEFAULT 0,
    input_tokens      BIGINT NOT NULL DEFAULT 0,
    output_tokens     BIGINT NOT NULL DEFAULT 0,
    total_credits     DECIMAL(20, 10) NOT NULL DEFAULT 0,
    reset_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO neonix_proxy_stats_baseline (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
