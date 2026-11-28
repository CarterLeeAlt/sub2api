-- Repair request logs only where the corresponding error records agree on the platform.
WITH matched AS (
    SELECT l.id, MIN(e.platform) AS platform
    FROM ops_system_logs l
    JOIN ops_error_logs e ON e.request_id = l.request_id AND e.api_key_id = l.api_key_id
      AND COALESCE(e.client_request_id, '') = COALESCE(l.client_request_id, '')
    WHERE (l.platform IS NULL OR l.platform = '')
      AND l.request_id <> '' AND e.platform <> ''
    GROUP BY l.id
    HAVING MIN(e.platform) = MAX(e.platform)
)
UPDATE ops_system_logs l
SET platform = matched.platform
FROM matched
WHERE l.id = matched.id AND (l.platform IS NULL OR l.platform = '');
