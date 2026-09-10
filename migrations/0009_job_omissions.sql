-- Keep what the agent said it could not take.
--
-- An agent reports two things about a payload beyond the targets: what it
-- had to leave out, and what it stored but a restore may not be able to
-- put back. Standalone mode has recorded both on the run since they were
-- introduced. The controller accepted them and threw them away, so a
-- fleet operator saw a job marked successful and nothing else -- and the
-- customer whose database was missing found out from the restore.

BEGIN;

ALTER TABLE backup_jobs ADD COLUMN missing  text[] NOT NULL DEFAULT '{}';
ALTER TABLE backup_jobs ADD COLUMN warnings text[] NOT NULL DEFAULT '{}';

COMMIT;
