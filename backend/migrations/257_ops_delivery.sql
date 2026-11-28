CREATE TABLE ops_delivery_requests (
    id uuid PRIMARY KEY, started_at timestamptz NOT NULL, ended_at timestamptz,
    request_id text NOT NULL, platform text NOT NULL, group_id bigint NOT NULL, model text NOT NULL,
    outcome text NOT NULL, first_ms double precision, span_ms double precision,
    chars bigint NOT NULL, tokens bigint NOT NULL, unknown_chars bigint NOT NULL,
    first_chars bigint NOT NULL, first_tokens bigint NOT NULL,
    attempts integer NOT NULL, upstream_errors integer NOT NULL, limited_attempts integer NOT NULL,
    incomplete_measurement boolean NOT NULL, user_id bigint, api_key_id bigint, account_id bigint, stream boolean NOT NULL
);
CREATE INDEX ops_delivery_started ON ops_delivery_requests(started_at);
CREATE INDEX ops_delivery_ended ON ops_delivery_requests(ended_at);
CREATE INDEX ops_delivery_dimensions ON ops_delivery_requests(group_id, platform, started_at);
CREATE TABLE ops_delivery_buckets (
    id uuid REFERENCES ops_delivery_requests(id) ON DELETE CASCADE,
    bucket timestamptz NOT NULL, chars bigint NOT NULL, tokens bigint NOT NULL, unknown_chars bigint NOT NULL,
    PRIMARY KEY (id,bucket)
);
CREATE INDEX ops_delivery_bucket_time ON ops_delivery_buckets(bucket);
CREATE TABLE ops_delivery_coverage (bucket timestamptz PRIMARY KEY);
