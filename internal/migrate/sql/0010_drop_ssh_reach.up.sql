-- Drop the `ssh` reach strategy (slice-41). See docs/architecture.md.
--
-- `ssh` was an assumption about how to reach a MiSTer: its root filesystem is
-- read-only, so the design assumed Syncthing could not run there and RetroSync
-- would have to SSH in and manipulate save files over SFTP. The MiSTer runs
-- Syncthing, so the assumption was wrong and the strategy never had a device.
--
-- It was never implemented — an `ssh` node resolved to
-- reach.ErrUnsupportedReach — but it was still offered in the /nodes reach
-- dropdown, so choosing it produced a node that silently never synced. Removing
-- the option removes the trap.
--
-- The Reach port itself is untouched: adding a transport later is still "write
-- an adapter and add a reach value", which was the point of the abstraction.

-- Refuse rather than rewrite. Any surviving ssh node never worked, but what to
-- do with it (repoint at a share, or delete it) is the operator's call, not a
-- migration's.
DO $$
DECLARE n int;
BEGIN
    SELECT count(*) INTO n FROM nodes WHERE reach = 'ssh';
    IF n > 0 THEN
        RAISE EXCEPTION
            'cannot drop the ssh reach strategy: % node(s) still use it. '
            'Those nodes never synced (the adapter was never built). Repoint '
            'them at a syncthing-share path, or delete them, then re-run.', n;
    END IF;
END $$;

ALTER TABLE nodes DROP CONSTRAINT nodes_reach_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_reach_check CHECK (reach IN ('syncthing-share'));
