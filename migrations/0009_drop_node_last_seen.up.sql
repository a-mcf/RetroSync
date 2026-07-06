-- Drop the node "last seen" feature (slice-24). See docs/data-model.md.
--
-- last_seen_at backed a persistent "reachable / not seen" badge derived purely
-- from whether the column was non-null. That badge was misleading: a node that
-- is syncing fine but was never smoke-tested showed "not seen", confusing
-- non-technical users. The on-demand smoke-test (a transient reachable/error
-- pill) stays; it just no longer writes last_seen. With no remaining reader, the
-- column is dropped.

ALTER TABLE nodes DROP COLUMN last_seen_at;
