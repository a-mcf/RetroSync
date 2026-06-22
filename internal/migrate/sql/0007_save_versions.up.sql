-- Save-version recovery net (slice-20). Before RetroSync overwrites ANY
-- member's save (during a propagation OR a conflict resolve), it snapshots the
-- about-to-be-overwritten bytes into a server-side, content-addressed blob store
-- plus a save_versions index. Restore picks a version -> it becomes the
-- authoritative save and propagates. This REPLACES the device-side
-- .retrosync-conflict-<ts> sibling backups (which cluttered device dirs and
-- replicated via Syncthing) with a clean central store.
--
-- Retention is COUNT-based (default 10), per (sync_id, node_id), ordered by the
-- monotonic `seq` bigserial (NOT a timestamp — a bad clock must never be able to
-- misorder/evict), deduped by content hash. captured_at is display-only.

-- save_blobs: content-addressed blob store. Identical content is stored once
-- (dedup by the hash PRIMARY KEY). Saves are tiny, so bytea in Postgres is fine.
CREATE TABLE save_blobs (
    hash text PRIMARY KEY,
    data bytea NOT NULL,
    size bigint NOT NULL
);

-- save_versions: one row per captured pre-overwrite snapshot of a member's save.
-- `seq` (bigserial) is the monotonic retention key — retention keeps the newest
-- N per (sync_id, node_id) ordered by seq DESC, never by captured_at.
CREATE TABLE save_versions (
    seq         bigserial PRIMARY KEY,
    sync_id     text NOT NULL REFERENCES syncs (id) ON DELETE CASCADE,
    node_id     text NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    hash        text NOT NULL REFERENCES save_blobs (hash),
    captured_at timestamptz NOT NULL DEFAULT now(),
    reason      text NOT NULL DEFAULT ''
);

-- Newest-first listing and retention pruning per (sync_id, node_id) by seq.
CREATE INDEX save_versions_sync_node_seq_idx
    ON save_versions (sync_id, node_id, seq DESC);

-- Orphan-blob GC and the blob-existence check (save_blobs.hash) join through
-- save_versions.hash; index it so the NOT EXISTS sweep and the FK-backed
-- existence probe don't seq-scan save_versions.
CREATE INDEX save_versions_hash_idx
    ON save_versions (hash);
