-- The repaired job belongs to the authenticated login role. Switch back to it
-- before removing the job. Do not recreate the prior non-login-owner job: it
-- could never execute, so restoring it would falsely claim retention coverage.
SET LOCAL ROLE NONE;

SELECT cron.unschedule('cliagg-retention-' || current_database())
WHERE EXISTS (
    SELECT 1 FROM cron.job
    WHERE jobname = 'cliagg-retention-' || current_database()
);
