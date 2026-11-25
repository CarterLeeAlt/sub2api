CREATE UNIQUE INDEX batch_image_jobs_owner_idempotency_uq
    ON batch_image_jobs (user_id, api_key_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';

DROP INDEX IF EXISTS batch_image_jobs_idempotency_key_idx;
