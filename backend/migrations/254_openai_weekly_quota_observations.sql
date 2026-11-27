-- Persist changes to the weekly reading and the evidence for each accounting boundary.
CREATE TABLE IF NOT EXISTS openai_weekly_quota_observations (
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    generation TEXT NOT NULL,
    period_start TIMESTAMPTZ,
    reason TEXT NOT NULL,
    start_source TEXT NOT NULL,
    previous_reading JSONB,
    current_reading JSONB,
    local_requests BIGINT NOT NULL,
    local_account_cost NUMERIC NOT NULL,
    PRIMARY KEY (account_id, generation)
);
