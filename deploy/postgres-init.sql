-- A database of its own for the audit chain.
--
-- The gateway will create gateway_audit inside it at startup and make it
-- append-only; nothing here needs to know the schema. What this file is for is
-- the separation: keys are current state and audit records are evidence, so
-- they want different retention, different backups and different grants, and
-- splitting them later means moving data rather than editing a DSN.
--
-- In a real deployment the gateway's role would also be narrowed here:
--
--   GRANT INSERT, SELECT ON gateway_audit TO gateway;
--   REVOKE UPDATE, DELETE, TRUNCATE ON gateway_audit FROM gateway;
--
-- It is not done in this file because the table does not exist until the
-- gateway's first boot, and because the owner of a table can restore its own
-- privileges — so the grant belongs with whoever owns the database, not with
-- the container that starts it. See docs/audit.md.
CREATE DATABASE audit OWNER gateway;

-- And a third for spend history, for a different reason: this one is the large
-- table. A row per request and a day rollup grow with traffic rather than with
-- administration, and it is the only one of the three with a retention setting,
-- so it wants its own storage, its own backup schedule and its own bad day.
CREATE DATABASE spend OWNER gateway;
