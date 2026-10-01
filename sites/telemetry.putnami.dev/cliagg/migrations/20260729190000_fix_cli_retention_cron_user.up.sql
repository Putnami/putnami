-- Migration connections assume the non-login database owner role. pg_cron
-- background workers must instead connect as the authenticated login role.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_roles
        WHERE rolname = session_user AND rolcanlogin
    ) THEN
        RAISE EXCEPTION
            'pg_cron session user % must be a login role', session_user
            USING ERRCODE = '55000';
    END IF;
END
$$;

SELECT cron.unschedule('cliagg-retention-' || current_database())
WHERE EXISTS (
    SELECT 1 FROM cron.job
    WHERE jobname = 'cliagg-retention-' || current_database()
);

SET LOCAL ROLE NONE;

SELECT cron.schedule_in_database(
    'cliagg-retention-' || current_database(),
    '0 0 * * *',
    'SELECT public.cli_expire_aggregates()',
    current_database()
);
