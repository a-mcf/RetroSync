-- Roll back the last_seen_at drop (slice-24). Restores the nullable timestamptz
-- column exactly as 0001_registry first defined it. No values are restored — the
-- column comes back empty (NULL) for every node, which is its original meaning of
-- "never observed".

ALTER TABLE nodes ADD COLUMN last_seen_at timestamptz NULL;
