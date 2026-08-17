-- Roll back the ssh-strategy drop (slice-41). Restores the CHECK constraint
-- exactly as 0001_registry defined it, so `ssh` is once again an accepted
-- column value.
--
-- This only widens the constraint. It does not restore the strategy: nothing
-- resolves an ssh node to an adapter, and store.ValidReach still rejects the
-- value, so the registry will not accept one through the API. A rollback is
-- for getting an older binary running against this database, not for making
-- ssh usable.

ALTER TABLE nodes DROP CONSTRAINT nodes_reach_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_reach_check CHECK (reach IN ('syncthing-share', 'ssh'));
