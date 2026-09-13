-- Backfill system-managed Codex fingerprint seeds for enabled OpenAI setup-token
-- accounts. 迁移 225 只覆盖 type = 'oauth'，而运行时判定
-- （IsOpenAIOAuthLike）同时接受 oauth 与 setup-token：setup-token 账号经 API
-- 直设 codex_fingerprint_mode 后模式生效但 seed 缺失，指纹收敛静默失效。
-- Idempotent: valid canonical seeds are preserved on rerun.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{codex_fingerprint_seed}',
    to_jsonb(gen_random_uuid()::text),
    true
)
WHERE deleted_at IS NULL
  AND platform = 'openai'
  AND type = 'setup-token'
  AND COALESCE(extra->>'codex_fingerprint_mode', '') IN ('device', 'session', 'full')
  AND (
      extra->>'codex_fingerprint_seed' IS NULL
      OR btrim(extra->>'codex_fingerprint_seed') = ''
      OR NOT (
          extra->>'codex_fingerprint_seed' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
          AND extra->>'codex_fingerprint_seed' <> '00000000-0000-0000-0000-000000000000'
      )
  );
