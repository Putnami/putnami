-- Intentionally non-reversible: partitioning is a one-way structural change
-- (the down path would require copying every row back to an unpartitioned
-- table, which we don't ship). When this migration is applied, `migrate down`
-- refuses with a clear error referencing the missing .down.sql sibling.
ALTER TABLE iam_users
    ADD COLUMN region TEXT NOT NULL DEFAULT 'eu';
